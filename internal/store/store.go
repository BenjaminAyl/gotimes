// Package store holds all SQL queries. Times are stored as unix seconds.
package store

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"time"
)

const maxItemsPerFeed = 200

type Store struct {
	db *sql.DB
	// wmu serializes feed writes so fetch workers don't fight over SQLite's write lock.
	wmu sync.Mutex
}

func New(db *sql.DB) *Store { return &Store{db: db} }

// UserID returns the id for email, creating the user on first sight.
func (s *Store) UserID(ctx context.Context, email string) (int64, error) {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO users (email, created_at) VALUES (?, ?) ON CONFLICT (email) DO NOTHING`,
		email, time.Now().Unix()); err != nil {
		return 0, err
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM users WHERE email = ?`, email).Scan(&id)
	return id, err
}

// FeedSub is a feed in a user's subscription list.
type FeedSub struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`      // what to show: your name for it, else the feed's own
	SiteTitle string `json:"site_title"` // the feed's own title
	URL       string `json:"url"`
	LastError string `json:"last_error"`
	Unread    int    `json:"unread"`
	Pinned    bool   `json:"pinned"`
	Category  int64  `json:"category_id"` // 0 = not filed
	IconV     int64  `json:"icon_v"`      // 0 = no icon; otherwise a version for /api/feeds/{id}/icon?v=
	Custom    bool   `json:"custom_icon"` // the icon is one you uploaded
}

// feedTitle is a feed's display name for the subscription alias s: your own
// name for it, else the feed's title, else its address.
const feedTitle = `COALESCE(NULLIF(s.custom_title, ''), NULLIF(f.title, ''), f.url)`

func (s *Store) Feeds(ctx context.Context, userID int64) ([]FeedSub, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT f.id, COALESCE(NULLIF(s.custom_title, ''), f.title), f.title, f.url, f.last_error, s.pinned,
		       COALESCE(s.category_id, 0),
		       CASE WHEN s.custom_icon IS NOT NULL THEN s.custom_icon_v
		            WHEN f.icon IS NULL THEN 0 ELSE f.icon_checked_at END,
		       s.custom_icon IS NOT NULL,
		       (SELECT COUNT(*) FROM items i
		         WHERE i.feed_id = f.id
		           AND NOT EXISTS (SELECT 1 FROM read_items r WHERE r.item_id = i.id AND r.user_id = ?))
		FROM feeds f JOIN subscriptions s ON s.feed_id = f.id
		WHERE s.user_id = ?
		ORDER BY s.pinned DESC, lower(`+feedTitle+`)`, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FeedSub
	for rows.Next() {
		var f FeedSub
		if err := rows.Scan(&f.ID, &f.Title, &f.SiteTitle, &f.URL, &f.LastError, &f.Pinned, &f.Category, &f.IconV, &f.Custom, &f.Unread); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Item is a feed entry. Content is sanitized HTML and only set by Store.Item.
type Item struct {
	ID        int64     `json:"id"`
	FeedID    int64     `json:"feed_id"`
	FeedTitle string    `json:"feed_title"`
	Title     string    `json:"title"`
	URL       string    `json:"url"`
	Summary   string    `json:"summary"`
	Author    string    `json:"author"`
	Content   string    `json:"content,omitempty"`
	Published time.Time `json:"published"`
	Read      bool      `json:"read"`
}

// itemQuery selects items the user is subscribed to. Pass "”" as contentCol
// for lists so article bodies are only read when one item is opened.
func itemQuery(contentCol string) string {
	return `SELECT i.id, i.feed_id, ` + feedTitle + `, i.title, i.url, i.summary, i.author, ` +
		contentCol + `, i.published_at,
		       EXISTS (SELECT 1 FROM read_items r WHERE r.item_id = i.id AND r.user_id = ?)
		FROM items i
		JOIN feeds f ON f.id = i.feed_id
		JOIN subscriptions s ON s.feed_id = i.feed_id AND s.user_id = ?`
}

func (s *Store) queryItems(ctx context.Context, query string, args ...any) ([]Item, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		var published int64
		if err := rows.Scan(&it.ID, &it.FeedID, &it.FeedTitle, &it.Title, &it.URL, &it.Summary, &it.Author,
			&it.Content, &published, &it.Read); err != nil {
			return nil, err
		}
		it.Published = time.Unix(published, 0).UTC()
		out = append(out, it)
	}
	return out, rows.Err()
}

// likeEscaper makes user text safe inside a LIKE pattern (with ESCAPE '\').
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// ItemFilter narrows Items. Zero values mean "no filter"; Limit is required.
type ItemFilter struct {
	FeedID     int64
	CategoryID int64
	UnreadOnly bool
	Text       string // matches title, summary, author or feed name
	Limit      int
}

// Items lists the user's newest items.
func (s *Store) Items(ctx context.Context, userID int64, f ItemFilter) ([]Item, error) {
	conds := []string{"1=1"}
	args := []any{userID, userID}
	if f.FeedID != 0 {
		conds = append(conds, "i.feed_id = ?")
		args = append(args, f.FeedID)
	}
	if f.CategoryID != 0 {
		conds = append(conds, "s.category_id = ?")
		args = append(args, f.CategoryID)
	}
	if f.UnreadOnly {
		conds = append(conds, "NOT EXISTS (SELECT 1 FROM read_items r WHERE r.item_id = i.id AND r.user_id = ?)")
		args = append(args, userID)
	}
	if f.Text != "" {
		pat := "%" + likeEscaper.Replace(f.Text) + "%"
		conds = append(conds, `(i.title LIKE ? ESCAPE '\' OR i.summary LIKE ? ESCAPE '\' OR i.author LIKE ? ESCAPE '\' OR `+feedTitle+` LIKE ? ESCAPE '\')`)
		args = append(args, pat, pat, pat, pat)
	}
	args = append(args, f.Limit)
	return s.queryItems(ctx, itemQuery("''")+" WHERE "+strings.Join(conds, " AND ")+
		" ORDER BY i.published_at DESC, i.id DESC LIMIT ?", args...)
}

// Item returns one item with its content, or sql.ErrNoRows if the user can't see it.
func (s *Store) Item(ctx context.Context, userID, itemID int64) (Item, error) {
	items, err := s.queryItems(ctx, itemQuery("i.content")+" WHERE i.id = ?", userID, userID, itemID)
	if err != nil {
		return Item{}, err
	}
	if len(items) == 0 {
		return Item{}, sql.ErrNoRows
	}
	return items[0], nil
}

// MarkRead marks one item read, only if the user is subscribed to its feed.
func (s *Store) MarkRead(ctx context.Context, userID, itemID int64) error {
	return s.markRead(ctx, userID, "i.id", itemID)
}

// MarkFeedRead marks every item in a subscribed feed read.
func (s *Store) MarkFeedRead(ctx context.Context, userID, feedID int64) error {
	return s.markRead(ctx, userID, "i.feed_id", feedID)
}

// markRead marks items read where the items column named by match equals id.
// match is always a constant from this file, never user input.
func (s *Store) markRead(ctx context.Context, userID int64, match string, id int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO read_items (user_id, item_id)
		SELECT ?, i.id FROM items i
		JOIN subscriptions s ON s.feed_id = i.feed_id AND s.user_id = ?
		WHERE `+match+` = ?`, userID, userID, id)
	return err
}

// Subscribe adds the feed (creating it if new) and subscribes the user.
// It returns the feed id.
func (s *Store) Subscribe(ctx context.Context, userID int64, url string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO feeds (url, next_fetch_at) VALUES (?, ?) ON CONFLICT (url) DO NOTHING`,
		url, time.Now().Unix()); err != nil {
		return 0, err
	}
	var feedID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM feeds WHERE url = ?`, url).Scan(&feedID); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO subscriptions (user_id, feed_id) VALUES (?, ?)`, userID, feedID); err != nil {
		return 0, err
	}
	return feedID, tx.Commit()
}

// TogglePin flips whether the section is pinned to the top for this user.
func (s *Store) TogglePin(ctx context.Context, userID, feedID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE subscriptions SET pinned = 1 - pinned WHERE user_id = ? AND feed_id = ?`, userID, feedID)
	return err
}

// Unsubscribe removes the user's subscription and drops feeds nobody follows.
func (s *Store) Unsubscribe(ctx context.Context, userID, feedID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM subscriptions WHERE user_id = ? AND feed_id = ?`, userID, feedID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM feeds WHERE id = ? AND NOT EXISTS (SELECT 1 FROM subscriptions WHERE feed_id = ?)`,
		feedID, feedID); err != nil {
		return err
	}
	return tx.Commit()
}

// FeedState is what the fetcher needs to poll a feed conditionally.
type FeedState struct {
	ID           int64
	URL          string
	ETag         string
	LastModified string
	Failures     int
}

// DueFeeds returns up to limit feeds whose next fetch time has passed.
func (s *Store) DueFeeds(ctx context.Context, now time.Time, limit int) ([]FeedState, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, url, etag, last_modified, failures FROM feeds
		WHERE next_fetch_at <= ? ORDER BY next_fetch_at LIMIT ?`, now.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FeedState
	for rows.Next() {
		var f FeedState
		if err := rows.Scan(&f.ID, &f.URL, &f.ETag, &f.LastModified, &f.Failures); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// RefreshUserFeeds makes all of the user's feeds due immediately.
func (s *Store) RefreshUserFeeds(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE feeds SET next_fetch_at = ?
		WHERE id IN (SELECT feed_id FROM subscriptions WHERE user_id = ?)`, time.Now().Unix(), userID)
	return err
}

// IconTarget is a feed whose icon should be (re)fetched.
type IconTarget struct {
	ID       int64
	URL      string // the feed address, used when the feed names no site
	SiteURL  string
	ImageURL string // the logo the feed itself declares, a fallback when the site blocks us
}

// IconsDue returns feeds whose icon was last checked before cutoff.
func (s *Store) IconsDue(ctx context.Context, cutoff time.Time, limit int) ([]IconTarget, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, url, site_url, image_url FROM feeds WHERE icon_checked_at < ? ORDER BY icon_checked_at LIMIT ?`,
		cutoff.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IconTarget
	for rows.Next() {
		var t IconTarget
		if err := rows.Scan(&t.ID, &t.URL, &t.SiteURL, &t.ImageURL); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SaveIcon records an icon check. Empty data keeps any icon already stored.
func (s *Store) SaveIcon(ctx context.Context, feedID int64, data []byte, contentType string, checked time.Time) error {
	if len(data) == 0 {
		_, err := s.db.ExecContext(ctx, `UPDATE feeds SET icon_checked_at = ? WHERE id = ?`, checked.Unix(), feedID)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE feeds SET icon = ?, icon_type = ?, icon_checked_at = ? WHERE id = ?`,
		data, contentType, checked.Unix(), feedID)
	return err
}

// FeedIcon returns the icon the user sees for a feed: the one they uploaded,
// else the site's. It returns sql.ErrNoRows if there is neither.
func (s *Store) FeedIcon(ctx context.Context, userID, feedID int64) ([]byte, string, error) {
	var data []byte
	var ct string
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(s.custom_icon, f.icon), CASE WHEN s.custom_icon IS NOT NULL THEN s.custom_icon_type ELSE f.icon_type END
		FROM feeds f JOIN subscriptions s ON s.feed_id = f.id AND s.user_id = ?
		WHERE f.id = ? AND COALESCE(s.custom_icon, f.icon) IS NOT NULL`, userID, feedID).Scan(&data, &ct)
	return data, ct, err
}

// RenameFeed sets the user's own name for a feed; an empty title goes back to the feed's own.
func (s *Store) RenameFeed(ctx context.Context, userID, feedID int64, title string) error {
	return s.updateSub(ctx, `UPDATE subscriptions SET custom_title = ? WHERE user_id = ? AND feed_id = ?`, title, userID, feedID)
}

// SetCustomIcon stores an icon the user uploaded for a feed.
func (s *Store) SetCustomIcon(ctx context.Context, userID, feedID int64, data []byte, contentType string) error {
	return s.updateSub(ctx,
		`UPDATE subscriptions SET custom_icon = ?, custom_icon_type = ?, custom_icon_v = ? WHERE user_id = ? AND feed_id = ?`,
		data, contentType, time.Now().UnixMicro(), userID, feedID)
}

// ClearCustomIcon goes back to the site's own icon.
func (s *Store) ClearCustomIcon(ctx context.Context, userID, feedID int64) error {
	return s.updateSub(ctx,
		`UPDATE subscriptions SET custom_icon = NULL, custom_icon_type = '', custom_icon_v = 0 WHERE user_id = ? AND feed_id = ?`,
		userID, feedID)
}

// updateSub runs an UPDATE on one subscription, returning sql.ErrNoRows if the user isn't subscribed.
func (s *Store) updateSub(ctx context.Context, query string, args ...any) error {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// NewItem is a parsed entry ready to insert.
type NewItem struct {
	GUID      string
	URL       string
	Title     string
	Summary   string
	Author    string
	Content   string
	Published time.Time
}

// FetchResult is the outcome of polling one feed.
type FetchResult struct {
	FeedID       int64
	NotModified  bool
	Err          error
	Title        string
	SiteURL      string
	ImageURL     string
	ETag         string
	LastModified string
	Items        []NewItem
}

// SaveFetch records a fetch outcome and schedules the next one.
func (s *Store) SaveFetch(ctx context.Context, r FetchResult, next time.Time) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	switch {
	case r.Err != nil:
		msg := r.Err.Error()
		if len(msg) > 300 {
			msg = msg[:300]
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE feeds SET last_error = ?, failures = failures + 1, next_fetch_at = ? WHERE id = ?`,
			msg, next.Unix(), r.FeedID)
	case r.NotModified:
		_, err = tx.ExecContext(ctx,
			`UPDATE feeds SET last_error = '', failures = 0, next_fetch_at = ? WHERE id = ?`,
			next.Unix(), r.FeedID)
	default:
		_, err = tx.ExecContext(ctx, `
			UPDATE feeds SET title = ?, site_url = ?, image_url = ?, etag = ?, last_modified = ?,
			       last_error = '', failures = 0, next_fetch_at = ? WHERE id = ?`,
			r.Title, r.SiteURL, r.ImageURL, r.ETag, r.LastModified, next.Unix(), r.FeedID)
		if err == nil {
			err = insertItems(ctx, tx, r.FeedID, r.Items)
		}
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func insertItems(ctx context.Context, tx *sql.Tx, feedID int64, items []NewItem) error {
	if len(items) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO items (feed_id, guid, url, title, summary, author, content, published_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (feed_id, guid) DO UPDATE SET
		    url = excluded.url, title = excluded.title, summary = excluded.summary,
		    author = excluded.author, content = excluded.content
		WHERE items.content != excluded.content OR items.title != excluded.title`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, it := range items {
		if _, err := stmt.ExecContext(ctx, feedID, it.GUID, it.URL, it.Title, it.Summary, it.Author, it.Content, it.Published.Unix()); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `
		DELETE FROM items WHERE feed_id = ? AND id NOT IN
		  (SELECT id FROM items WHERE feed_id = ? ORDER BY published_at DESC, id DESC LIMIT ?)`,
		feedID, feedID, maxItemsPerFeed)
	return err
}
