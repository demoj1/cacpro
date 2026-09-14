// cacpro — тупой кэширующий reverse-proxy: один апстрим, правила по пути,
// кэш — файлы на диске в раскладке URL.
package main

import (
	"flag"
	"fmt"
	"hash/maphash"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const forever = time.Duration(-1)

type rule struct {
	re    *regexp.Regexp
	ttl   time.Duration // forever — иммутабельно, 0 — не кэшировать
	async bool          // просроченное отдаём сразу, обновляем в фоне
}

type server struct {
	upstream string // без хвостового слэша
	prefix   string // <cache-dir>/<host апстрима>
	def      rule
	rules    []rule
	client   *http.Client
	proxy    *httputil.ReverseProxy
	seed     maphash.Seed
	locks    [64]sync.Mutex
}

func main() {
	log.SetFlags(0)
	s := &server{seed: maphash.MakeSeed()}
	upstream := flag.String("upstream", "", "origin, напр. https://repo.hex.pm (обязательно)")
	cacheDir := flag.String("cache-dir", "/cache", "куда класть кэш")
	listen := flag.String("listen", ":8087", "адрес для входящих")

	// Флаги позиционные: --match открывает правило, следующие --ttl/--async-update — про него.
	// До первого --match они задают дефолт для несматченных путей.
	cur := &s.def
	flag.Func("match", "regex по пути (целиком), открывает правило", func(v string) error {
		re, err := regexp.Compile("^(?:" + v + ")$")
		if err != nil {
			return err
		}
		s.rules = append(s.rules, rule{re: re, ttl: s.def.ttl, async: s.def.async})
		cur = &s.rules[len(s.rules)-1]
		return nil
	})
	flag.Func("ttl", "срок жизни для текущего правила: 5m, 1h, forever, 0 = не кэшировать", func(v string) error {
		if v == "forever" {
			cur.ttl = forever
			return nil
		}
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return fmt.Errorf("плохой ttl %q", v)
		}
		cur.ttl = d
		return nil
	})
	flag.BoolFunc("async-update", "для текущего правила: отдать просроченное сразу, обновить в фоне", func(string) error {
		cur.async = true
		return nil
	})
	flag.Parse()

	u, err := url.Parse(*upstream)
	if err != nil || u.Scheme == "" || u.Host == "" {
		log.Fatalf("--upstream: нужен полный URL, получено %q", *upstream)
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	s.upstream = u.String()
	s.prefix = filepath.Join(*cacheDir, u.Host)
	s.client = &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 30 * time.Second}}
	s.proxy = httputil.NewSingleHostReverseProxy(u)
	director := s.proxy.Director
	s.proxy.Director = func(r *http.Request) {
		director(r)
		r.Host = u.Host
	}

	log.Printf("cacpro: %s → %s, кэш в %s, правил: %d", *listen, s.upstream, s.prefix, len(s.rules))
	log.Fatal((&http.Server{Addr: *listen, Handler: s, ReadHeaderTimeout: 10 * time.Second}).ListenAndServe())
}

func (s *server) match(p string) rule {
	for _, r := range s.rules {
		if r.re.MatchString(p) {
			return r
		}
	}
	return s.def
}

type upstreamStatus int

func (u upstreamStatus) Error() string {
	return "апстрим ответил " + strconv.Itoa(int(u))
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "only GET/HEAD", http.StatusMethodNotAllowed)
		return
	}
	p := r.URL.Path
	if p == "" || p[0] != '/' {
		p = "/" + p
	}
	p = path.Clean(p)
	rl := s.match(p)
	if rl.ttl == 0 {
		s.proxy.ServeHTTP(w, r)
		logReq(r, 0, "PASS", start)
		return
	}

	key := p
	if r.URL.RawQuery != "" {
		key += "?" + url.QueryEscape(r.URL.RawQuery)
	}
	file := s.prefix + filepath.FromSlash(key)

	f, st := open(file)
	fresh := f != nil && (rl.ttl == forever || time.Since(st.ModTime()) < rl.ttl)

	src := "HIT"
	switch {
	case fresh:
	case f != nil && rl.async:
		src = "STALE"
		go s.refresh(r.URL.RequestURI(), file)
	default:
		err := s.fetch(r.URL.RequestURI(), file)
		if us, ok := err.(upstreamStatus); ok {
			w.WriteHeader(int(us))
			logReq(r, int(us), "UPSTREAM", start)
			return
		}
		switch {
		case err == nil:
			src = "MISS"
			if f != nil {
				f.Close()
			}
			if f, st = open(file); f == nil {
				http.Error(w, "cache write failed", http.StatusInternalServerError)
				return
			}
		case f != nil:
			src = "STALE-ERR"
			log.Printf("апстрим недоступен, отдаём просроченное %s: %v", key, err)
		default:
			http.Error(w, err.Error(), http.StatusBadGateway)
			logReq(r, http.StatusBadGateway, "ERR", start)
			return
		}
	}

	http.ServeContent(w, r, p, st.ModTime(), f)
	f.Close()
	logReq(r, http.StatusOK, src, start)
}

// open возвращает открытый обычный файл и его stat, либо nil.
func open(file string) (*os.File, os.FileInfo) {
	f, err := os.Open(file)
	if err != nil {
		return nil, nil
	}
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		f.Close()
		return nil, nil
	}
	return f, st
}

func (s *server) refresh(uri, file string) {
	if err := s.fetch(uri, file); err != nil {
		log.Printf("фоновое обновление %s: %v", uri, err)
	}
}

// fetch качает URI апстрима в file атомарно (tmp + rename). Одновременные промахи по одному
// ключу выстраиваются за замком: кто пришёл вторым, видит уже свежий файл и не качает.
func (s *server) fetch(uri, file string) error {
	started := time.Now()
	mu := &s.locks[maphash.String(s.seed, file)%uint64(len(s.locks))]
	mu.Lock()
	defer mu.Unlock()

	if st, err := os.Stat(file); err == nil && st.ModTime().After(started) {
		return nil
	}

	resp, err := s.client.Get(s.upstream + uri)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return upstreamStatus(resp.StatusCode)
	}

	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), "."+filepath.Base(file)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), file)
}

// accessLog — *os.File, а не io.Writer: через интерфейс буфер утёк бы в heap.
var accessLog = os.Stderr

// logReq пишет строку доступа без fmt и без аллокаций: буфер на стеке, append'ы, один write.
func logReq(r *http.Request, status int, src string, start time.Time) {
	var arr [512]byte
	b := append(arr[:0], r.Method...)
	b = append(b, ' ')
	b = append(b, r.URL.Path...)
	if r.URL.RawQuery != "" {
		b = append(b, '?')
		b = append(b, r.URL.RawQuery...)
	}
	b = append(b, ' ')
	if status != 0 {
		b = strconv.AppendInt(b, int64(status), 10)
		b = append(b, ' ')
	}
	b = append(b, src...)
	b = append(b, ' ')
	b = strconv.AppendInt(b, int64(time.Since(start)/time.Microsecond), 10)
	b = append(b, "us\n"...)
	accessLog.Write(b)
}
