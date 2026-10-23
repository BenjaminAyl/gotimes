// Package server is the HTTP layer: Cloudflare Access identity, HTMX pages.
package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/benaylward/gotimes/internal/feeds"
	"github.com/benaylward/gotimes/internal/store"
)

// Cloudflare Access sets this on every request it lets through.
const accessEmailHeader = "Cf-Access-Authenticated-User-Email"

const itemLimit = 100

type Config struct {
	// DevEmail, if set, is used when the Access header is absent. Local dev only.
	DevEmail string
}

type Server struct {
	cfg     Config
	store   *store.Store
	fetcher *feeds.Fetcher
	sched   *feeds.Scheduler
	tmpl    *template.Template
	static  fs.FS
	assetV  string   // content hash of static/, appended to asset URLs
	users   sync.Map // email -> user id
}

func New(cfg Config, st *store.Store, f *feeds.Fetcher, sched *feeds.Scheduler, webFS fs.FS) (*Server, error) {
	tmpl, err := template.ParseFS(webFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	static, err := fs.Sub(webFS, "static")
	if err != nil {
		return nil, err
	}
	v, err := assetVersion(static)
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, store: st, fetcher: f, sched: sched, tmpl: tmpl, static: static, assetV: v}, nil
}

// assetVersion hashes every static file. Asset URLs carry it (?v=...), so a
// changed file is a new URL and can never be served stale by a browser or by
// Cloudflare's edge cache, which caches .css/.js by default.
func assetVersion(static fs.FS) (string, error) {
	h := sha256.New()
	err := fs.WalkDir(static, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(static, p)
		if err != nil {
			return err
		}
		h.Write([]byte(p))
		h.Write(b)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:10], err
}

// serveStatic serves embedded assets. Versioned URLs and fonts never change
// under the same URL, so they cache forever; anything else must revalidate.
func (s *Server) serveStatic() http.Handler {
	files := http.StripPrefix("/static/", http.FileServerFS(s.static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") != "" || strings.HasPrefix(r.URL.Path, "/static/fonts/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.Handle("GET /static/", s.serveStatic())

	mux.Handle("GET /{$}", s.auth(s.page))
	mux.Handle("GET /feeds", s.auth(s.sectionList))
	mux.Handle("POST /feeds", s.auth(s.addFeed))
	mux.Handle("DELETE /feeds/{id}", s.auth(s.removeFeed))
	mux.Handle("POST /feeds/{id}/read", s.auth(s.readFeed))
	mux.Handle("POST /feeds/{id}/pin", s.auth(s.pinFeed))
	mux.Handle("GET /items", s.auth(s.itemList))
	mux.Handle("GET /items/{id}", s.auth(s.reader))
	mux.Handle("POST /items/{id}/read", s.auth(s.readItem))
	mux.Handle("POST /refresh", s.auth(s.refresh))
	return secure(mux)
}

// secure sets headers that keep feed content inert: no scripts but our own
// (so no inline handlers either), and images/fonts/styles from known places.
func secure(next http.Handler) http.Handler {
	const csp = "default-src 'none'; script-src 'self'; style-src 'self'; font-src 'self'; " +
		"img-src * data:; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

type ctxKey struct{}

// auth identifies the user from the Cloudflare Access header. That header is
// only trustworthy because the app is reachable solely through the tunnel,
// behind an Access policy. Never publish the port directly.
func (s *Server) auth(next func(http.ResponseWriter, *http.Request, int64)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		email := strings.ToLower(strings.TrimSpace(r.Header.Get(accessEmailHeader)))
		if email == "" {
			email = strings.ToLower(s.cfg.DevEmail)
		}
		if email == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// State changes must come from htmx: a cross-site form post can't set
		// custom headers, which blocks CSRF without tokens.
		if r.Method != http.MethodGet && r.Header.Get("HX-Request") != "true" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		uid, err := s.userID(r.Context(), email)
		if err != nil {
			s.fail(w, err)
			return
		}
		next(w, r, uid)
	})
}

func (s *Server) userID(ctx context.Context, email string) (int64, error) {
	if v, ok := s.users.Load(email); ok {
		return v.(int64), nil
	}
	id, err := s.store.UserID(ctx, email)
	if err != nil {
		return 0, err
	}
	s.users.Store(email, id)
	return id, nil
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	log.Printf("error: %v", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

type pageData struct {
	Today string
	V     string // asset version
}

func (s *Server) page(w http.ResponseWriter, r *http.Request, _ int64) {
	s.render(w, "index", pageData{Today: time.Now().Format("Monday, January 2, 2006"), V: s.assetV})
}

// view is the sidebar filter, carried on every request as ?feed=ID&unread=1.
type view struct {
	Feed   int64
	Unread bool
	Story  int64 // story currently open in the reader
	Q      string
}

func viewOf(r *http.Request) view {
	q := r.URL.Query()
	feed, _ := strconv.ParseInt(q.Get("feed"), 10, 64)
	story, _ := strconv.ParseInt(q.Get("story"), 10, 64)
	return view{Feed: feed, Unread: q.Get("unread") == "1", Story: story, Q: strings.TrimSpace(q.Get("q"))}
}

type sectionsData struct {
	Feeds       []store.FeedSub
	TotalUnread int
	AnyPinned   bool
	View        view
}

func (s *Server) sectionList(w http.ResponseWriter, r *http.Request, uid int64) {
	list, err := s.store.Feeds(r.Context(), uid)
	if err != nil {
		s.fail(w, err)
		return
	}
	d := sectionsData{Feeds: list, View: viewOf(r)}
	for _, f := range list {
		d.TotalUnread += f.Unread
		d.AnyPinned = d.AnyPinned || f.Pinned
	}
	s.render(w, "sections", d)
}

// trigger tells the page which parts to refresh (htmx HX-Trigger header).
func trigger(w http.ResponseWriter, events ...string) {
	w.Header().Set("HX-Trigger", strings.Join(events, ", "))
}

// addFeed validates the URL by fetching it once, so only real feeds are stored
// and the new section's stories show up immediately.
func (s *Server) addFeed(w http.ResponseWriter, r *http.Request, uid int64) {
	raw := strings.TrimSpace(r.FormValue("url"))
	if u, err := url.Parse(raw); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		s.render(w, "notice", "Enter a full web address, starting with http:// or https://")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	res := s.fetcher.Fetch(ctx, store.FeedState{URL: raw})
	if res.Err != nil {
		s.render(w, "notice", "Couldn’t read that feed: "+res.Err.Error())
		return
	}

	feedID, err := s.store.Subscribe(ctx, uid, raw)
	if err != nil {
		s.fail(w, err)
		return
	}
	res.FeedID = feedID
	if err := s.store.SaveFetch(ctx, res, time.Now().Add(30*time.Minute)); err != nil {
		s.fail(w, err)
		return
	}
	trigger(w, "feeds-changed", "section-added")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removeFeed(w http.ResponseWriter, r *http.Request, uid int64) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := s.store.Unsubscribe(r.Context(), uid, id); err != nil {
		s.fail(w, err)
		return
	}
	trigger(w, "feeds-changed")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) pinFeed(w http.ResponseWriter, r *http.Request, uid int64) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := s.store.TogglePin(r.Context(), uid, id); err != nil {
		s.fail(w, err)
		return
	}
	trigger(w, "sections-changed")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) readFeed(w http.ResponseWriter, r *http.Request, uid int64) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := s.store.MarkFeedRead(r.Context(), uid, id); err != nil {
		s.fail(w, err)
		return
	}
	trigger(w, "feeds-changed")
	w.WriteHeader(http.StatusNoContent)
}

// itemView adds presentation to a story for the list.
type itemView struct {
	store.Item
	Selected bool
}

func (i itemView) ShortDate() string {
	if i.Published.Year() == time.Now().Year() {
		return i.Published.Format("Jan 2")
	}
	return i.Published.Format("Jan 2, 2006")
}

type itemsData struct {
	Rows []itemView
	Q    string
}

func (s *Server) itemList(w http.ResponseWriter, r *http.Request, uid int64) {
	v := viewOf(r)
	items, err := s.store.Items(r.Context(), uid, v.Feed, v.Unread, v.Q, itemLimit)
	if err != nil {
		s.fail(w, err)
		return
	}
	rows := make([]itemView, len(items))
	for i, it := range items {
		rows[i] = itemView{Item: it, Selected: it.ID == v.Story}
	}
	s.render(w, "items", itemsData{Rows: rows, Q: v.Q})
}

type readerData struct {
	Item store.Item
	Date string
	Body template.HTML // sanitized at ingest (internal/feeds)
}

// reader renders one story in the main pane.
func (s *Server) reader(w http.ResponseWriter, r *http.Request, uid int64) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	it, err := s.store.Item(r.Context(), uid, id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "reader", readerData{
		Item: it,
		Date: it.Published.Format("Monday, January 2, 2006 · 15:04 UTC"),
		Body: template.HTML(it.Content), //nolint:gosec // sanitized when stored
	})
}

// readItem records that the story was opened and returns its refreshed list row.
func (s *Server) readItem(w http.ResponseWriter, r *http.Request, uid int64) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := s.store.MarkRead(r.Context(), uid, id); err != nil {
		s.fail(w, err)
		return
	}
	it, err := s.store.Item(r.Context(), uid, id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	it.Content = ""
	trigger(w, "read-changed")
	s.render(w, "item", itemView{Item: it, Selected: true})
}

// refresh fetches the user's feeds now and waits, so the page reloads with
// whatever is new.
func (s *Server) refresh(w http.ResponseWriter, r *http.Request, uid int64) {
	if err := s.store.RefreshUserFeeds(r.Context(), uid); err != nil {
		s.fail(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	s.sched.RefreshNow(ctx)
	trigger(w, "feeds-changed")
	w.WriteHeader(http.StatusNoContent)
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil
}
