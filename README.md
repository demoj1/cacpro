# cacpro

A dumb caching reverse proxy. One upstream, path-based rules, cache is plain files on disk
laid out like the URL (`<cache-dir>/<host>/<path>/_`), so `ls`, `du` and `rm` are the admin UI.

Single static binary, stdlib only, ~7 MB container image built `FROM scratch`.

## Why

Package registries (Hex, npm, PyPI, apt, …) serve immutable artifacts plus a small mutable
index. General-purpose caches (nginx, Varnish, Squid) can do this, but you pay for a config
language to express two rules. `cacpro` expresses exactly those two rules and nothing else.

## Usage

```
cacpro --listen :8087 --cache-dir /cache --upstream https://repo.hex.pm \
  --match '/tarballs/.*'                   --ttl forever \
  --match '/names|/versions|/packages/.*'  --ttl 5m --async-update
```

Flags are **positional**: `--match` opens a rule, and the `--ttl` / `--async-update` that follow
belong to it. A `--ttl` placed *before* the first `--match` is the default for paths that match
no rule; without a default, unmatched paths are proxied straight through, uncached.

| flag | meaning |
|---|---|
| `--upstream URL` | origin to proxy (required) |
| `--listen ADDR` | listen address, default `:8087` |
| `--cache-dir DIR` | where to store files, default `/cache` |
| `--match REGEX` | regex over the request path, anchored on both ends; opens a rule |
| `--ttl DUR` | `5m`, `1h`, `forever` (immutable), `0` = don't cache |
| `--async-update` | serve a stale entry immediately and refresh it in the background |

Rules are checked in order; the first match wins.

## Behaviour

- Only `GET` and `HEAD`; everything else gets `405`.
- Only `200` responses are stored. `404` and other statuses pass through and are never cached.
- Expired entry, no `--async-update`: refetch synchronously, then serve.
- Expired entry, `--async-update`: serve the old file at once, refresh in the background.
- Upstream unreachable: serve whatever is on disk (stale) rather than fail.
- Concurrent misses for the same path make a single upstream request; the others wait.
- Writes are atomic (`tmp` + `rename`), so a crash never leaves a half-written file.
- `Content-Length`, `Last-Modified`, `HEAD` and `Range` come for free from `http.ServeContent`.
- Access log is one line per request, no `fmt`, no allocations:
  `GET /tarballs/jason-1.4.5.tar 200 HIT 26us`. Sources: `HIT`, `MISS`, `STALE`,
  `STALE-ERR`, `PASS`, `UPSTREAM` (non-200 relayed), `ERR`.

There is no eviction. Artifacts are small; if you ever need to reclaim space,
`find /cache -name _ -atime +365 -delete` does the job.

## Examples

### Hex (Elixir / Erlang)

```
cacpro --upstream https://repo.hex.pm \
  --match '/tarballs/.*'                                --ttl forever \
  --match '/names|/versions|/packages/.*|/installs/.*'  --ttl 5m --async-update
```

Clients:

```
HEX_MIRROR=http://cache-host:8087 mix deps.get
# or persistently
mix hex.config mirror_url http://cache-host:8087
```

Registry signatures are verified by the Hex client against hex.pm's public key; the proxy
serves the same bytes, so nothing needs re-signing.

### Several upstreams

One upstream per instance. Run one `cacpro` per registry; it is 7 MB. Registries that split
index and files across two hosts (PyPI: `pypi.org/simple` + `files.pythonhosted.org`) need two
instances, and the client must be told about both.

### Docker / Podman

```
podman build -t cacpro .
podman run -d --name hex-cache -p 8087:8087 -v ./cache:/cache:Z cacpro \
  --upstream https://repo.hex.pm \
  --match '/tarballs/.*' --ttl forever \
  --match '/names|/versions|/packages/.*|/installs/.*' --ttl 5m --async-update
```

`docker-compose.yml`:

```yaml
services:
  hex-cache:
    image: cacpro
    restart: always
    ports: ["8087:8087"]
    volumes: ["./hex-cache:/cache"]
    command:
      - --upstream=https://repo.hex.pm
      - --match=/tarballs/.*
      - --ttl=forever
      - --match=/names|/versions|/packages/.*|/installs/.*
      - --ttl=5m
      - --async-update
```

Check it works:

```
$ curl -sI http://localhost:8087/tarballs/jason-1.4.5.tar >/dev/null; podman logs hex-cache | tail -1
GET /tarballs/jason-1.4.5.tar 200 MISS 110145us
$ curl -sI http://localhost:8087/tarballs/jason-1.4.5.tar >/dev/null; podman logs hex-cache | tail -1
GET /tarballs/jason-1.4.5.tar 200 HIT 62us
```

## Cache layout

Every URL path becomes a directory and the body is the file `_` inside it:

```
/cache/repo.hex.pm/packages/jason/_
/cache/repo.hex.pm/tarballs/jason-1.4.5.tar/_
/cache/registry.npmjs.org/lodash/_                      # metadata
/cache/registry.npmjs.org/lodash/-/lodash-4.17.21.tgz/_ # tarball
```

The trailing `_` is what lets `/a` and `/a/b` both be cached — registries like npm serve
content at both. A query string, if any, is escaped into the directory name.

## Limits

- The cache key is path + query, nothing else: no `Vary`, no per-header caching.
  This is a registry cache, not a CDN.

## Performance

A cache hit costs 5 heap allocations in this code (open the file, build its path) plus what
`net/http` itself needs; the body goes out via `sendfile`. GC is a non-issue: heap stays at a
few MB. If you still want it quieter, `GOGC=off GOMEMLIMIT=64MiB` in the environment works.

```
go test -bench . -benchmem
```

## Development

```
go build && go vet ./... && go test -bench . -benchmem
```
