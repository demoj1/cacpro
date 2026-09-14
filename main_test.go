package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func hitServer(b *testing.B) (*server, *http.Request) {
	dir := b.TempDir()
	s := &server{
		upstream: "https://repo.hex.pm",
		prefix:   filepath.Join(dir, "repo.hex.pm"),
		rules:    []rule{{re: regexp.MustCompile(`^(?:/tarballs/.*)$`), ttl: forever}},
	}
	f := filepath.Join(s.prefix, "tarballs", "jason-1.4.4.tar", "_")
	os.MkdirAll(filepath.Dir(f), 0o755)
	os.WriteFile(f, make([]byte, 26112), 0o644)
	accessLog, _ = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	return s, httptest.NewRequest("GET", "/tarballs/jason-1.4.4.tar", nil)
}

func BenchmarkHit(b *testing.B) {
	s, req := hitServer(b)
	b.ReportAllocs()
	for b.Loop() {
		s.ServeHTTP(httptest.NewRecorder(), req)
	}
}

// Шум харнесса: сколько стоит сам NewRecorder, чтобы вычесть из BenchmarkHit.
func BenchmarkRecorderOnly(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		httptest.NewRecorder()
	}
}
