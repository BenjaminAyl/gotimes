package feeds

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/benaylward/rss/internal/store"
)

func TestSummarizeStripsMarkup(t *testing.T) {
	got := summarize(`<p>Hello <b>world</b> &amp; <script>x</script>friends</p>`)
	if strings.ContainsAny(got, "<>") || !strings.Contains(got, "Hello world & ") {
		t.Fatalf("got %q", got)
	}
	if n := len([]rune(summarize(strings.Repeat("a", 2000)))); n != maxSummary+1 {
		t.Fatalf("len %d", n)
	}
}

func TestNextFetchBacksOff(t *testing.T) {
	now := time.Now()
	if d := nextFetch(now, 0, false).Sub(now); d != refreshEvery {
		t.Fatalf("success interval %v", d)
	}
	if d := nextFetch(now, 2, true).Sub(now); d != 4*refreshEvery {
		t.Fatalf("backoff %v", d)
	}
	if d := nextFetch(now, 50, true).Sub(now); d != maxBackoff {
		t.Fatalf("cap %v", d)
	}
}

func TestFetcherBlocksPrivateAddresses(t *testing.T) {
	res := NewFetcher(false).Fetch(context.Background(), store.FeedState{URL: "http://127.0.0.1:1/feed"})
	if res.Err == nil || !strings.Contains(res.Err.Error(), "non-public") {
		t.Fatalf("expected block, got %v", res.Err)
	}
}

func TestSanitizeStripsActiveContent(t *testing.T) {
	in := `<p onclick="x()">hi</p><script>alert(1)</script><img src="http://a/b.png" onerror="x()">` +
		`<a href="javascript:alert(1)">bad</a><iframe src="http://evil"></iframe><a href="http://ok.example/">ok</a>`
	got := sanitize(in)
	for _, bad := range []string{"<script", "onclick", "onerror", "javascript:", "<iframe"} {
		if strings.Contains(got, bad) {
			t.Fatalf("%q survived in %q", bad, got)
		}
	}
	if !strings.Contains(got, "hi") || !strings.Contains(got, `href="http://ok.example/"`) {
		t.Fatalf("lost legitimate content: %q", got)
	}
	if !strings.Contains(got, `rel="`) || !strings.Contains(got, `target="_blank"`) {
		t.Fatalf("links should open safely in a new tab: %q", got)
	}
}

func TestAlternateLinks(t *testing.T) {
	page := `<html><head>
		<link rel="stylesheet" href="/x.css">
		<link rel="alternate" type="application/rss+xml" title="News" href="/news/rss">
		<link rel="alternate" type="text/html" href="/other">
		<link rel="Alternate" type="application/atom+xml" href="https://elsewhere.test/atom">
	</head></html>`
	got := alternateLinks([]byte(page), "https://example.test/forever/news?;rss")
	want := []string{"https://example.test/news/rss", "https://elsewhere.test/atom"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDiscoverFollowsAdvertisedFeed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/page", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`<html><head><link rel="alternate" type="application/rss+xml" href="/feed.xml"></head></html>`))
	})
	mux.HandleFunc("/feed.xml", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>Real</title><item><title>One</title><guid>1</guid></item></channel></rss>`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, res := NewFetcher(true).Discover(context.Background(), store.FeedState{URL: srv.URL + "/page"})
	if res.Err != nil || got != srv.URL+"/feed.xml" || res.Title != "Real" || len(res.Items) != 1 {
		t.Fatalf("url=%s err=%v title=%q items=%d", got, res.Err, res.Title, len(res.Items))
	}
}

var tinyPNG = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89")

func TestIconFromDeclaredLinkThenFavicon(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`<html><head><link rel="apple-touch-icon" href="/touch.png"><link rel="shortcut icon" href="/i.png"></head></html>`))
	})
	mux.HandleFunc("/i.png", func(w http.ResponseWriter, _ *http.Request) { w.Write(tinyPNG) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	data, ct := NewFetcher(true).Icon(context.Background(), srv.URL+"/", "")
	if ct != "image/png" || len(data) != len(tinyPNG) {
		t.Fatalf("ct=%q len=%d", ct, len(data))
	}

	// no declared icon: fall back to /favicon.ico; an HTML error page is not an icon
	bare := http.NewServeMux()
	bare.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("<html></html>")) })
	bare.HandleFunc("/favicon.ico", func(w http.ResponseWriter, _ *http.Request) { w.Write(tinyPNG) })
	srv2 := httptest.NewServer(bare)
	defer srv2.Close()
	if _, ct := NewFetcher(true).Icon(context.Background(), srv2.URL+"/", ""); ct != "image/png" {
		t.Fatalf("favicon fallback ct=%q", ct)
	}
	none := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("<html>not an image</html>")) }))
	defer none.Close()
	if data, _ := NewFetcher(true).Icon(context.Background(), none.URL+"/", ""); data != nil {
		t.Fatalf("html served as icon: %q", data)
	}
}

func TestIconFoundInPageLargerThanTheReadLimit(t *testing.T) {
	big := append([]byte(`<html><head><link rel="icon" href="/i.png"></head><body>`), make([]byte, 2<<20)...)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.Write(big) })
	mux.HandleFunc("/i.png", func(w http.ResponseWriter, _ *http.Request) { w.Write(tinyPNG) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	if _, ct := NewFetcher(true).Icon(context.Background(), srv.URL+"/", ""); ct != "image/png" {
		t.Fatalf("ct=%q: a long page must be read up to the limit, not rejected", ct)
	}
}

func TestIconFallsBackToFeedLogoThenService(t *testing.T) {
	// a site that blocks everything except the feed's own logo path
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "blocked", http.StatusForbidden) })
	mux.HandleFunc("/logo.png", func(w http.ResponseWriter, _ *http.Request) { w.Write(tinyPNG) })
	mux.HandleFunc("/svc/", func(w http.ResponseWriter, _ *http.Request) { w.Write(tinyPNG) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "blocked", http.StatusForbidden) }))
	defer blocked.Close()

	f := NewFetcher(true)
	if _, ct := f.Icon(context.Background(), srv.URL+"/news", srv.URL+"/logo.png"); ct != "image/png" {
		t.Fatalf("feed logo fallback ct=%q", ct)
	}
	// nothing from the site or the feed, and no service configured: no icon, and no third party asked
	if data, _ := f.Icon(context.Background(), blocked.URL+"/", ""); data != nil {
		t.Fatalf("got an icon from a blocked site")
	}
	f.IconService = srv.URL + "/svc/%s"
	if _, ct := f.Icon(context.Background(), blocked.URL+"/", ""); ct != "image/png" {
		t.Fatalf("service fallback ct=%q", ct)
	}
}
