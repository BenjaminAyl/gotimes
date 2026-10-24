package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/benaylward/rss/internal/db"
	"github.com/benaylward/rss/internal/feeds"
	"github.com/benaylward/rss/internal/store"
	"github.com/benaylward/rss/web"
)

const sampleFeed = `<?xml version="1.0"?><rss version="2.0"><channel><title>Desk</title><link>http://127.0.0.1:1/</link>
<item><title>Hello</title><link>http://x/1</link><guid>1</guid><description>Body</description></item></channel></rss>`

func newTestServer(t *testing.T, dev string) http.Handler {
	t.Helper()
	conn, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := db.Migrate(conn); err != nil {
		t.Fatal(err)
	}
	st := store.New(conn)
	f := feeds.NewFetcher(true) // the test feed lives on loopback
	srv, err := New(Config{DevEmail: dev}, st, f, feeds.NewScheduler(st, f, 1), web.FS)
	if err != nil {
		t.Fatal(err)
	}
	return srv.Handler()
}

func do(h http.Handler, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAuthAndCSRFGuards(t *testing.T) {
	h := newTestServer(t, "")
	if w := do(h, "GET", "/healthz", "", nil); w.Code != 200 {
		t.Fatalf("healthz = %d", w.Code)
	}
	if w := do(h, "GET", "/api/feeds", "", nil); w.Code != 401 {
		t.Fatalf("no identity = %d, want 401", w.Code)
	}
	who := map[string]string{accessEmailHeader: "me@example.com"}
	if w := do(h, "GET", "/api/feeds", "", who); w.Code != 200 {
		t.Fatalf("with identity = %d", w.Code)
	}
	if w := do(h, "POST", "/api/refresh", "", who); w.Code != 403 {
		t.Fatalf("POST without client header = %d, want 403", w.Code)
	}
	if w := do(h, "GET", "/api/feeds", "", who); w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", w.Header().Get("Cache-Control"))
	}
}

func TestSubscribeListReadFlow(t *testing.T) {
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write([]byte(sampleFeed))
	}))
	defer feed.Close()

	h := newTestServer(t, "")
	me := map[string]string{accessEmailHeader: "me@example.com", clientHeader: "test"}

	if w := do(h, "POST", "/api/feeds", `{"url":"ftp://nope"}`, me); w.Code != 400 {
		t.Fatalf("bad url = %d", w.Code)
	}
	w := do(h, "POST", "/api/feeds", `{"url":"`+feed.URL+`"}`, me)
	if w.Code != 201 {
		t.Fatalf("add = %d %s", w.Code, w.Body)
	}

	var items struct{ Items []store.Item }
	w = do(h, "GET", "/api/items?unread=1", "", me)
	if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil || len(items.Items) != 1 {
		t.Fatalf("items = %s (%v)", w.Body, err)
	}
	id := items.Items[0].ID

	for q, want := range map[string]string{"hell": `"Hello"`, "zzz": `"items":[]`, "%": `"items":[]`} {
		if w := do(h, "GET", "/api/items?q="+url.QueryEscape(q), "", me); !strings.Contains(w.Body.String(), want) {
			t.Fatalf("search %q = %s", q, w.Body)
		}
	}

	if w := do(h, "POST", "/api/items/"+itoa(id)+"/read", "", me); w.Code != 204 {
		t.Fatalf("read = %d", w.Code)
	}
	w = do(h, "GET", "/api/items?unread=1", "", me)
	if !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Fatalf("unread after read = %s", w.Body)
	}

	var fl struct{ Feeds []store.FeedSub }
	w = do(h, "GET", "/api/feeds", "", me)
	if err := json.Unmarshal(w.Body.Bytes(), &fl); err != nil || len(fl.Feeds) != 1 || fl.Feeds[0].Unread != 0 {
		t.Fatalf("feeds = %s (%v)", w.Body, err)
	}
	fid := itoa(fl.Feeds[0].ID)
	if w := do(h, "POST", "/api/feeds/"+fid+"/pin", "", me); w.Code != 204 {
		t.Fatalf("pin = %d", w.Code)
	}
	if w := do(h, "GET", "/api/feeds", "", me); !strings.Contains(w.Body.String(), `"pinned":true`) {
		t.Fatalf("not pinned: %s", w.Body)
	}
	if w := do(h, "POST", "/api/feeds/nope/pin", "", me); w.Code != 404 {
		t.Fatalf("bad id = %d", w.Code)
	}

	// another user sees nothing of mine
	other := map[string]string{accessEmailHeader: "other@example.com"}
	if w := do(h, "GET", "/api/items/"+itoa(id), "", other); w.Code != 404 {
		t.Fatalf("other user's item = %d, want 404", w.Code)
	}
	if w := do(h, "GET", "/api/items/"+itoa(id), "", me); w.Code != 200 || !strings.Contains(w.Body.String(), `"title":"Hello"`) {
		t.Fatalf("own item = %d %s", w.Code, w.Body)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestUnsubscribeRemovesFeed(t *testing.T) {
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(sampleFeed)) }))
	defer feed.Close()
	h := newTestServer(t, "me@example.com")
	hdr := map[string]string{clientHeader: "test"}

	w := do(h, "POST", "/api/feeds", `{"url":"`+feed.URL+`"}`, hdr)
	var added struct{ ID int64 }
	if err := json.Unmarshal(w.Body.Bytes(), &added); err != nil || added.ID == 0 {
		t.Fatalf("add = %d %s", w.Code, w.Body)
	}
	if w := do(h, "DELETE", "/api/feeds/"+itoa(added.ID), "", hdr); w.Code != 204 {
		t.Fatalf("delete = %d", w.Code)
	}
	if w := do(h, "GET", "/api/feeds", "", nil); !strings.Contains(w.Body.String(), `"feeds":[]`) {
		t.Fatalf("after delete: %s", w.Body)
	}
}

func TestFrontendServedWithVersionedAssets(t *testing.T) {
	h := newTestServer(t, "")
	w := do(h, "GET", "/", "", nil)
	body := w.Body.String()
	if w.Code != 200 || strings.Contains(body, "__V__") || !strings.Contains(body, "/assets/app.js?v=") {
		t.Fatalf("index = %d\n%s", w.Code, body)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("index Cache-Control = %q", w.Header().Get("Cache-Control"))
	}
	if w := do(h, "GET", "/assets/app.js?v=abc", "", nil); w.Code != 200 || w.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("versioned asset = %d %q", w.Code, w.Header().Get("Cache-Control"))
	}
	if w := do(h, "GET", "/assets/app.js", "", nil); w.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("unversioned asset Cache-Control = %q", w.Header().Get("Cache-Control"))
	}
}

func TestFeedIconServed(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89")
	var site *httptest.Server
	site = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/feed.xml":
			w.Write([]byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>Iconic</title><link>` + site.URL + `/</link><item><title>One</title><guid>1</guid></item></channel></rss>`))
		case "/icon.png":
			w.Write(png)
		default:
			w.Write([]byte(`<html><head><link rel="icon" href="/icon.png"></head></html>`))
		}
	}))
	defer site.Close()

	h := newTestServer(t, "me@example.com")
	hdr := map[string]string{clientHeader: "test"}
	if w := do(h, "POST", "/api/feeds", `{"url":"`+site.URL+`/feed.xml"}`, hdr); w.Code != 201 {
		t.Fatalf("add = %d %s", w.Code, w.Body)
	}
	var fl struct{ Feeds []store.FeedSub }
	w := do(h, "GET", "/api/feeds", "", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &fl); err != nil || len(fl.Feeds) != 1 || fl.Feeds[0].IconV == 0 {
		t.Fatalf("feeds = %s (%v)", w.Body, err)
	}
	w = do(h, "GET", "/api/feeds/"+itoa(fl.Feeds[0].ID)+"/icon", "", nil)
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || w.Body.Len() != len(png) {
		t.Fatalf("icon = %d %q %d bytes", w.Code, w.Header().Get("Content-Type"), w.Body.Len())
	}
	if w := do(h, "GET", "/api/feeds/9999/icon", "", nil); w.Code != 404 {
		t.Fatalf("missing icon = %d", w.Code)
	}
}

func TestCategories(t *testing.T) {
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(sampleFeed)) }))
	defer feed.Close()
	h := newTestServer(t, "me@example.com")
	hdr := map[string]string{clientHeader: "test"}

	if w := do(h, "GET", "/api/icons", "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"1"`) {
		t.Fatalf("icons = %d %s", w.Code, w.Body)
	}
	if w := do(h, "POST", "/api/categories", `{"name":"News","icon":"nope"}`, hdr); w.Code != 400 {
		t.Fatalf("unknown icon = %d", w.Code)
	}
	if w := do(h, "POST", "/api/categories", `{"name":"  ","icon":"1"}`, hdr); w.Code != 400 {
		t.Fatalf("blank name = %d", w.Code)
	}
	w := do(h, "POST", "/api/categories", `{"name":"News","icon":"1"}`, hdr)
	var created struct{ ID int64 }
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || w.Code != 201 || created.ID == 0 {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	cat := itoa(created.ID)
	if w := do(h, "POST", "/api/categories", `{"name":"News","icon":"2"}`, hdr); w.Code != 409 {
		t.Fatalf("duplicate = %d", w.Code)
	}

	w = do(h, "POST", "/api/feeds", `{"url":"`+feed.URL+`"}`, hdr)
	var added struct{ ID int64 }
	json.Unmarshal(w.Body.Bytes(), &added)
	fid := itoa(added.ID)
	if w := do(h, "PUT", "/api/feeds/"+fid+"/category", `{"category_id":`+cat+`}`, hdr); w.Code != 204 {
		t.Fatalf("file feed = %d %s", w.Code, w.Body)
	}
	if w := do(h, "PUT", "/api/feeds/"+fid+"/category", `{"category_id":9999}`, hdr); w.Code != 404 {
		t.Fatalf("file into missing category = %d", w.Code)
	}
	var fl struct {
		Feeds      []store.FeedSub
		Categories []store.Category
	}
	w = do(h, "GET", "/api/feeds", "", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &fl); err != nil || len(fl.Categories) != 1 ||
		fl.Feeds[0].Category != created.ID || fl.Categories[0].Icon != "1" {
		t.Fatalf("feeds = %s (%v)", w.Body, err)
	}
	if w := do(h, "GET", "/api/items?category="+cat, "", nil); !strings.Contains(w.Body.String(), `"Hello"`) {
		t.Fatalf("category items = %s", w.Body)
	}
	if w := do(h, "GET", "/api/items?category=9999", "", nil); !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Fatalf("other category items = %s", w.Body)
	}

	if w := do(h, "POST", "/api/categories/"+cat+"/pin", "", hdr); w.Code != 204 {
		t.Fatalf("pin = %d", w.Code)
	}
	if w := do(h, "PATCH", "/api/categories/"+cat, `{"name":"Daily","icon":"2"}`, hdr); w.Code != 204 {
		t.Fatalf("rename = %d", w.Code)
	}
	if w := do(h, "GET", "/api/feeds", "", nil); !strings.Contains(w.Body.String(), `"name":"Daily","icon":"2","pinned":true`) {
		t.Fatalf("after edit = %s", w.Body)
	}

	// someone else can't touch or use my category
	other := map[string]string{accessEmailHeader: "other@example.com", clientHeader: "test"}
	if w := do(h, "DELETE", "/api/categories/"+cat, "", other); w.Code != 204 {
		t.Fatalf("other delete = %d", w.Code) // a no-op for them
	}
	if w := do(h, "GET", "/api/feeds", "", nil); !strings.Contains(w.Body.String(), `"Daily"`) {
		t.Fatalf("category vanished: %s", w.Body)
	}

	if w := do(h, "DELETE", "/api/categories/"+cat, "", hdr); w.Code != 204 {
		t.Fatalf("delete = %d", w.Code)
	}
	w = do(h, "GET", "/api/feeds", "", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &fl); err != nil || len(fl.Categories) != 0 || fl.Feeds[0].Category != 0 {
		t.Fatalf("after delete = %s", w.Body)
	}
}

func TestRenameAndCustomIcon(t *testing.T) {
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(sampleFeed)) }))
	defer feed.Close()
	h := newTestServer(t, "me@example.com")
	hdr := map[string]string{clientHeader: "test"}
	w := do(h, "POST", "/api/feeds", `{"url":"`+feed.URL+`"}`, hdr)
	var added struct{ ID int64 }
	json.Unmarshal(w.Body.Bytes(), &added)
	fid := itoa(added.ID)

	list := func() store.FeedSub {
		var fl struct{ Feeds []store.FeedSub }
		json.Unmarshal(do(h, "GET", "/api/feeds", "", nil).Body.Bytes(), &fl)
		return fl.Feeds[0]
	}
	if f := list(); f.Title != "Desk" || f.SiteTitle != "Desk" || f.Custom {
		t.Fatalf("before = %+v", f)
	}

	// rename: shown everywhere, the feed's own title is kept alongside
	if w := do(h, "PATCH", "/api/feeds/"+fid, `{"title":"  My desk  "}`, hdr); w.Code != 204 {
		t.Fatalf("rename = %d %s", w.Code, w.Body)
	}
	if f := list(); f.Title != "My desk" || f.SiteTitle != "Desk" {
		t.Fatalf("renamed = %+v", f)
	}
	if w := do(h, "GET", "/api/items", "", nil); !strings.Contains(w.Body.String(), `"feed_title":"My desk"`) {
		t.Fatalf("items don't use the new name: %s", w.Body)
	}
	if w := do(h, "GET", "/api/items?q=my+desk", "", nil); !strings.Contains(w.Body.String(), `"Hello"`) {
		t.Fatalf("search by new name: %s", w.Body)
	}
	if w := do(h, "PATCH", "/api/feeds/"+fid, `{"title":""}`, hdr); w.Code != 204 {
		t.Fatalf("reset name = %d", w.Code)
	}
	if f := list(); f.Title != "Desk" {
		t.Fatalf("reset = %+v", f)
	}
	if w := do(h, "PATCH", "/api/feeds/9999", `{"title":"x"}`, hdr); w.Code != 404 {
		t.Fatalf("rename missing = %d", w.Code)
	}

	// upload: PNG by its bytes, SVG only when it says so and looks like one, HTML never
	png := "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89"
	put := func(body, ctype string) int {
		return do(h, "PUT", "/api/feeds/"+fid+"/icon", body, map[string]string{clientHeader: "t", "Content-Type": ctype}).Code
	}
	if c := put("<html><script>alert(1)</script></html>", "image/png"); c != 415 {
		t.Fatalf("html as png = %d, want 415", c)
	}
	if c := put("<html></html>", "image/svg+xml"); c != 415 {
		t.Fatalf("html as svg = %d, want 415", c)
	}
	if c := put(strings.Repeat("x", maxIconUpload+10), "image/png"); c != 413 {
		t.Fatalf("huge = %d, want 413", c)
	}
	if c := put(png, "image/png"); c != 204 {
		t.Fatalf("png = %d", c)
	}
	f := list()
	if !f.Custom || f.IconV == 0 {
		t.Fatalf("after upload = %+v", f)
	}
	icon := do(h, "GET", "/api/feeds/"+fid+"/icon", "", nil)
	if icon.Code != 200 || icon.Header().Get("Content-Type") != "image/png" || icon.Body.String() != png {
		t.Fatalf("icon = %d %q", icon.Code, icon.Header().Get("Content-Type"))
	}
	if c := put(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1 1"/>`, "image/svg+xml"); c != 204 {
		t.Fatalf("svg = %d", c)
	}
	if ct := do(h, "GET", "/api/feeds/"+fid+"/icon", "", nil).Header().Get("Content-Type"); ct != "image/svg+xml" {
		t.Fatalf("svg served as %q", ct)
	}

	// another user doesn't see my icon or name, and can't change them
	other := map[string]string{accessEmailHeader: "other@example.com", clientHeader: "t"}
	if w := do(h, "PUT", "/api/feeds/"+fid+"/icon", png, other); w.Code != 404 {
		t.Fatalf("other user upload = %d, want 404", w.Code)
	}

	if w := do(h, "DELETE", "/api/feeds/"+fid+"/icon", "", hdr); w.Code != 204 {
		t.Fatalf("clear = %d", w.Code)
	}
	if f := list(); f.Custom {
		t.Fatalf("still custom: %+v", f)
	}
}
