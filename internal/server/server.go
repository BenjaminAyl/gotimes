// Package server is the HTTP layer: Cloudflare Access identity, a JSON API and the embedded frontend.
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/benaylward/rss/internal/feeds"
	"github.com/benaylward/rss/internal/store"
)

// Cloudflare Access sets this on every request it lets through.
const accessEmailHeader = "Cf-Access-Authenticated-User-Email"

// Non-GET requests must carry this header. A cross-site page can't set a
// custom header without a CORS preflight, which this server never grants, so
// it blocks CSRF without tokens.
const clientHeader = "X-Requested-With"

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
	ui      *frontend
	users   sync.Map // email -> user id
}

func New(cfg Config, st *store.Store, f *feeds.Fetcher, sched *feeds.Scheduler, webFS fs.FS) (*Server, error) {
	ui, err := newFrontend(webFS)
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, store: st, fetcher: f, sched: sched, ui: ui}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /{$}", s.ui.serveIndex)
	mux.HandleFunc("GET /assets/", s.ui.serveAssets)

	mux.Handle("GET /api/feeds", s.auth(s.listFeeds))
	mux.Handle("POST /api/feeds", s.auth(s.addFeed))
	mux.Handle("DELETE /api/feeds/{id}", s.auth(s.action(s.store.Unsubscribe)))
	mux.Handle("POST /api/feeds/{id}/read", s.auth(s.action(s.store.MarkFeedRead)))
	mux.Handle("POST /api/feeds/{id}/pin", s.auth(s.action(s.store.TogglePin)))
	mux.Handle("GET /api/feeds/{id}/icon", s.auth(s.feedIcon))
	mux.Handle("GET /api/items", s.auth(s.listItems))
	mux.Handle("GET /api/items/{id}", s.auth(s.getItem))
	mux.Handle("POST /api/items/{id}/read", s.auth(s.action(s.store.MarkRead)))
	mux.Handle("POST /api/refresh", s.auth(s.refresh))
	return secure(mux)
}

func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// Feed HTML is sanitized, and this policy keeps it inert anyway: only our own script and styles run.
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; "+
			"img-src * data:; media-src *; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

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
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if r.Method != http.MethodGet && r.Header.Get(clientHeader) == "" {
			writeError(w, http.StatusForbidden, "missing "+clientHeader+" header")
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	log.Printf("error: %v", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return 0, false
	}
	return id, true
}

// action wraps a store call on the {id} in the path that answers 204.
func (s *Server) action(do func(ctx context.Context, userID, id int64) error) func(http.ResponseWriter, *http.Request, int64) {
	return func(w http.ResponseWriter, r *http.Request, uid int64) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		if err := do(r.Context(), uid, id); err != nil {
			s.fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// GET /api/feeds: the user's subscriptions, pinned first.
func (s *Server) listFeeds(w http.ResponseWriter, r *http.Request, uid int64) {
	list, err := s.store.Feeds(r.Context(), uid)
	if err != nil {
		s.fail(w, err)
		return
	}
	if list == nil {
		list = []store.FeedSub{}
	}
	total := 0
	for _, f := range list {
		total += f.Unread
	}
	writeJSON(w, http.StatusOK, map[string]any{"feeds": list, "total_unread": total})
}

// POST /api/feeds {"url": "..."}: validates by fetching once, so only real
// feeds are stored and the new feed's items are there immediately. A web page
// address works too: the feed it advertises is subscribed instead.
func (s *Server) addFeed(w http.ResponseWriter, r *http.Request, uid int64) {
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "expected JSON body {\"url\": ...}")
		return
	}
	raw := strings.TrimSpace(body.URL)
	if u, err := url.Parse(raw); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		writeError(w, http.StatusBadRequest, "url must start with http:// or https://")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	raw, res := s.fetcher.Discover(ctx, store.FeedState{URL: raw})
	if res.Err != nil {
		writeError(w, http.StatusUnprocessableEntity, "couldn't read that feed: "+res.Err.Error())
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
	// best effort and quick: the scheduler retries later if this misses
	iconCtx, iconCancel := context.WithTimeout(ctx, 5*time.Second)
	defer iconCancel()
	feeds.FillIcon(iconCtx, s.store, s.fetcher, store.IconTarget{ID: feedID, URL: raw, SiteURL: res.SiteURL})
	writeJSON(w, http.StatusCreated, map[string]any{"id": feedID})
}

// GET /api/feeds/{id}/icon: the feed's icon. URLs carry ?v= (see FeedSub.IconV), so they never change.
func (s *Server) feedIcon(w http.ResponseWriter, r *http.Request, _ int64) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	data, ct, err := s.store.FeedIcon(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "no icon")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Write(data)
}

// GET /api/items?feed=ID&unread=1&q=text: newest first, without article bodies.
func (s *Server) listItems(w http.ResponseWriter, r *http.Request, uid int64) {
	q := r.URL.Query()
	feed, _ := strconv.ParseInt(q.Get("feed"), 10, 64)
	items, err := s.store.Items(r.Context(), uid, feed, q.Get("unread") == "1", strings.TrimSpace(q.Get("q")), itemLimit)
	if err != nil {
		s.fail(w, err)
		return
	}
	if items == nil {
		items = []store.Item{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// GET /api/items/{id}: one item including its sanitized HTML content.
func (s *Server) getItem(w http.ResponseWriter, r *http.Request, uid int64) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	it, err := s.store.Item(r.Context(), uid, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

// POST /api/refresh fetches the user's feeds now and waits for the sweep.
func (s *Server) refresh(w http.ResponseWriter, r *http.Request, uid int64) {
	if err := s.store.RefreshUserFeeds(r.Context(), uid); err != nil {
		s.fail(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	s.sched.RefreshNow(ctx)
	w.WriteHeader(http.StatusNoContent)
}
