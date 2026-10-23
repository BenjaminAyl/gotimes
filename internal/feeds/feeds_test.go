package feeds

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/benaylward/gotimes/internal/store"
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
