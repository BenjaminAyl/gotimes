// Package server is the HTTP layer: Cloudflare Access identity, a JSON API and the embedded frontend.
package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
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
	mux.Handle("PUT /api/feeds/{id}/category", s.auth(s.setFeedCategory))
	mux.Handle("PATCH /api/feeds/{id}", s.auth(s.renameFeed))
	mux.Handle("GET /api/feeds/{id}/icon", s.auth(s.feedIcon))
	mux.Handle("PUT /api/feeds/{id}/icon", s.auth(s.uploadFeedIcon))
	mux.Handle("DELETE /api/feeds/{id}/icon", s.auth(s.action(s.store.ClearCustomIcon)))
	mux.Handle("POST /api/categories", s.auth(s.createCategory))
	mux.Handle("PATCH /api/categories/{id}", s.auth(s.updateCategory))
	mux.Handle("DELETE /api/categories/{id}", s.auth(s.action(s.store.DeleteCategory)))
	mux.Handle("POST /api/categories/{id}/pin", s.auth(s.action(s.store.TogglePinCategory)))
	mux.Handle("POST /api/categories/{id}/read", s.auth(s.action(s.store.MarkCategoryRead)))
	mux.Handle("GET /api/icons", s.auth(func(w http.ResponseWriter, _ *http.Request, _ int64) {
		writeJSON(w, http.StatusOK, map[string]any{"icons": s.ui.icons})
	}))
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
			"img-src * data: blob:; media-src *; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
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
		s.finish(w, do(r.Context(), uid, id))
	}
}

// GET /api/feeds: the user's subscriptions (pinned first) and categories.
func (s *Server) listFeeds(w http.ResponseWriter, r *http.Request, uid int64) {
	list, err := s.store.Feeds(r.Context(), uid)
	if err != nil {
		s.fail(w, err)
		return
	}
	cats, err := s.store.Categories(r.Context(), uid)
	if err != nil {
		s.fail(w, err)
		return
	}
	if list == nil {
		list = []store.FeedSub{}
	}
	if cats == nil {
		cats = []store.Category{}
	}
	total := 0
	for _, f := range list {
		total += f.Unread
	}
	writeJSON(w, http.StatusOK, map[string]any{"feeds": list, "categories": cats, "total_unread": total})
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
	iconCtx, iconCancel := context.WithTimeout(ctx, 8*time.Second)
	defer iconCancel()
	feeds.FillIcon(iconCtx, s.store, s.fetcher, store.IconTarget{ID: feedID, URL: raw, SiteURL: res.SiteURL, ImageURL: res.ImageURL})
	writeJSON(w, http.StatusCreated, map[string]any{"id": feedID})
}

// GET /api/feeds/{id}/icon: the feed's icon. URLs carry ?v= (see FeedSub.IconV), so they never change.
func (s *Server) feedIcon(w http.ResponseWriter, r *http.Request, uid int64) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	data, ct, err := s.store.FeedIcon(r.Context(), uid, id)
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

// GET /api/items?feed=ID&category=ID&unread=1&q=text: newest first, without article bodies.
func (s *Server) listItems(w http.ResponseWriter, r *http.Request, uid int64) {
	q := r.URL.Query()
	feed, _ := strconv.ParseInt(q.Get("feed"), 10, 64)
	cat, _ := strconv.ParseInt(q.Get("category"), 10, 64)
	items, err := s.store.Items(r.Context(), uid, store.ItemFilter{
		FeedID: feed, CategoryID: cat, UnreadOnly: q.Get("unread") == "1",
		Text: strings.TrimSpace(q.Get("q")), Limit: itemLimit,
	})
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

const maxCategoryName = 40

// categoryBody reads and validates {"name": ..., "icon": ...}.
func (s *Server) categoryBody(w http.ResponseWriter, r *http.Request) (name, icon string, ok bool) {
	var body struct {
		Name string `json:"name"`
		Icon string `json:"icon"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "expected JSON body {\"name\": ..., \"icon\": ...}")
		return "", "", false
	}
	name = strings.TrimSpace(body.Name)
	switch {
	case name == "" || len([]rune(name)) > maxCategoryName:
		writeError(w, http.StatusBadRequest, "name must be 1 to 40 characters")
	case body.Icon != "" && !s.ui.hasIcon(body.Icon):
		writeError(w, http.StatusBadRequest, "unknown icon")
	default:
		return name, body.Icon, true
	}
	return "", "", false
}

func (s *Server) createCategory(w http.ResponseWriter, r *http.Request, uid int64) {
	name, icon, ok := s.categoryBody(w, r)
	if !ok {
		return
	}
	id, err := s.store.CreateCategory(r.Context(), uid, name, icon)
	if errors.Is(err, store.ErrCategoryExists) {
		writeError(w, http.StatusConflict, "you already have a category with that name")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (s *Server) updateCategory(w http.ResponseWriter, r *http.Request, uid int64) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	name, icon, ok := s.categoryBody(w, r)
	if !ok {
		return
	}
	err := s.store.UpdateCategory(r.Context(), uid, id, name, icon)
	if errors.Is(err, store.ErrCategoryExists) {
		writeError(w, http.StatusConflict, "that name is taken, or the category doesn't exist")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PUT /api/feeds/{id}/category {"category_id": N}: file a feed under a category; 0 un-files it.
func (s *Server) setFeedCategory(w http.ResponseWriter, r *http.Request, uid int64) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		CategoryID int64 `json:"category_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "expected JSON body {\"category_id\": ...}")
		return
	}
	err := s.store.SetFeedCategory(r.Context(), uid, id, body.CategoryID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "no such feed or category")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

const (
	maxFeedName   = 100
	maxIconUpload = 256 << 10
)

// PATCH /api/feeds/{id} {"title": "..."}: your own name for the feed; empty resets it.
func (s *Server) renameFeed(w http.ResponseWriter, r *http.Request, uid int64) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "expected JSON body {\"title\": ...}")
		return
	}
	title := strings.TrimSpace(body.Title)
	if len([]rune(title)) > maxFeedName {
		writeError(w, http.StatusBadRequest, "name must be at most 100 characters")
		return
	}
	s.finish(w, s.store.RenameFeed(r.Context(), uid, id, title))
}

// PUT /api/feeds/{id}/icon: the body is the image itself (png, jpeg, gif, webp, ico or svg).
func (s *Server) uploadFeedIcon(w http.ResponseWriter, r *http.Request, uid int64) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxIconUpload))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "image must be under 256KB")
		return
	}
	ct := uploadType(data, r.Header.Get("Content-Type"))
	if ct == "" {
		writeError(w, http.StatusUnsupportedMediaType, "that isn't a PNG, JPEG, GIF, WebP, ICO or SVG image")
		return
	}
	s.finish(w, s.store.SetCustomIcon(r.Context(), uid, id, data, ct))
}

// uploadType decides what an uploaded file is from its bytes, not from what the
// client claims. SVG has no signature to sniff, so it must say so and look like one.
func uploadType(data []byte, declared string) string {
	ct := http.DetectContentType(data)
	switch {
	case strings.HasPrefix(ct, "image/"):
		return ct
	case strings.HasPrefix(declared, "image/svg+xml") && bytes.Contains(bytes.ToLower(data[:min(len(data), 2048)]), []byte("<svg")):
		return "image/svg+xml"
	}
	return ""
}

// finish answers 204, 404 for an unknown feed, or 500.
func (s *Server) finish(w http.ResponseWriter, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "no such feed")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
