package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func TestAssetVersionChangesWithContent(t *testing.T) {
	a, _ := assetVersion(fstest.MapFS{"app.css": {Data: []byte("a{}")}})
	b, _ := assetVersion(fstest.MapFS{"app.css": {Data: []byte("b{}")}})
	again, _ := assetVersion(fstest.MapFS{"app.css": {Data: []byte("a{}")}})
	if a == b || a != again || len(a) != 10 {
		t.Fatalf("a=%s b=%s again=%s", a, b, again)
	}
}

func TestStaticCacheHeaders(t *testing.T) {
	s := &Server{static: fstest.MapFS{
		"app.css":       {Data: []byte("a{}")},
		"fonts/x.woff2": {Data: []byte("f")},
		"favicon.svg":   {Data: []byte("<svg/>")},
	}}
	h := secure(s.serveStatic())
	cases := map[string]string{
		"/static/app.css?v=abc": "public, max-age=31536000, immutable",
		"/static/fonts/x.woff2": "public, max-age=31536000, immutable",
		"/static/app.css":       "no-cache",
		"/static/favicon.svg":   "no-cache",
	}
	for path, want := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if got := rec.Header().Get("Cache-Control"); got != want {
			t.Errorf("%s: Cache-Control %q, want %q", path, got, want)
		}
	}
	// dynamic responses are never cached
	rec := httptest.NewRecorder()
	secure(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("dynamic Cache-Control %q", got)
	}
}
