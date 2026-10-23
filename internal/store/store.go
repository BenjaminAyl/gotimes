// Package store holds all SQL queries. Times are stored as unix seconds.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"
)

const maxItemsPerFeed = 200

type Store struct {
	db *sql.DB
	// wmu serializes the large feed-write transactions so concurrent fetch
	// workers don't spend their time fighting over SQLite's write lock.
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

// FeedSub is a feed as shown in a user's subscription list.
type FeedSub struct {
	ID        int64
	Title     string
	URL       string
	LastError string
	Unread    int
	Pinned    bool
}

func (f FeedSub) Name() string {
	if f.Title != "" {
		return f.Title
	}
	return f.URL
}

func (s *Store) Feeds(ctx context.Context, userID int64) ([]FeedSub, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT f.id, f.title, f.url, f.last_error, s.pinned,
		       (SELECT COUNT(*) FROM items i
		         WHERE i.feed_id = f.id
		           AND NOT EXISTS (SELECT 1 FROM read_items r WHERE r.item_id = i.id AND r.user_id = ?))
		FROM feeds f JOIN subscriptions s ON s.feed_id = f.id
		WHERE s.user_id = ?
		ORDER BY s.pinned DESC, lower(CASE WHEN f.title = '' THEN f.url ELSE f.title END)`, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FeedSub
	for rows.Next() {
		var f FeedSub
		if err := rows.Scan(&f.ID, &f.Title, &f.URL, &f.LastError, &f.Pinned, &f.Unread); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Item is an entry as shown in the reader.
type Item struct {
	ID        int64
	FeedTitle string
	Title     string
	URL       string
	Summary   string
	Author    string
	Content   string // sanitized HTML; only loaded by Item, not Items
	Published time.Time
	Read      bool
}

// itemCols is the column list; the list view substitutes ” for content so
// article bodies aren't read until one is opened.
const itemCols = `i.id, f.title, f.url, i.title, i.url, i.summary, i.author, %s, i.published_at,
	       EXISTS (SELECT 1 FROM read_items r WHERE r.item_id = i.id AND r.user_id = ?)`

const itemFrom = `
	FROM items i
	JOIN feeds f ON f.id = i.feed_id
	JOIN subscriptions s ON s.feed_id = i.feed_id AND s.user_id = ?`

func itemSelect(withContent bool) string {
	content := "''"
	if withContent {
		content = "i.content"
	}
	return "SELECT " + fmt.Sprintf(itemCols, content) + itemFrom
}

func scanItems(rows *sql.Rows) ([]Item, error) {
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		var feedTitle, feedURL string
		var ts int64
		if err := rows.Scan(&it.ID, &feedTitle, &feedURL, &it.Title, &it.URL, &it.Summary, &it.Author, &it.Content, &ts, &it.Read); err != nil {
			return nil, err
		}
		it.FeedTitle = feedTitle
		if it.FeedTitle == "" {
			it.FeedTitle = feedURL
		}
		it.Published = time.Unix(ts, 0).UTC()
		out = append(out, it)
	}
	return out, rows.Err()
}

// likeEscaper makes user text safe inside a LIKE pattern (with ESCAPE '\').
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// Items lists the user's newest items, optionally limited to one feed and/or
// to those whose title, summary, author or feed name contain q.
func (s *Store) Items(ctx context.Context, userID, feedID int64, unreadOnly bool, text string, limit int) ([]Item, error) {
	query := itemSelect(false) + ` WHERE 1=1`
	args := []any{userID, userID}
	if feedID != 0 {
		query += ` AND i.feed_id = ?`
		args = append(args, feedID)
	}
	if unreadOnly {
		query += ` AND NOT EXISTS (SELECT 1 FROM read_items r WHERE r.item_id = i.id AND r.user_id = ?)`
		args = append(args, userID)
	}
	if text != "" {
		pat := "%" + likeEscaper.Replace(text) + "%"
		query += ` AND (i.title LIKE ? ESCAPE '\' OR i.summary LIKE ? ESCAPE '\' OR i.author LIKE ? ESCAPE '\' OR f.title LIKE ? ESCAPE '\')`
		args = append(args, pat, pat, pat, pat)
	}
	query += ` ORDER BY i.published_at DESC, i.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return scanItems(rows)
}

func (s *Store) Item(ctx context.Context, userID, itemID int64) (Item, error) {
	rows, err := s.db.QueryContext(ctx, itemSelect(true)+` WHERE i.id = ?`, userID, userID, itemID)
	if err != nil {
		return Item{}, err
	}
	items, err := scanItems(rows)
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
	_, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO read_items (user_id, item_id)
		SELECT ?, i.id FROM items i
		JOIN subscriptions s ON s.feed_id = i.feed_id AND s.user_id = ?
		WHERE i.id = ?`, userID, userID, itemID)
	return err
}

// MarkFeedRead marks every item in a subscribed feed read.
func (s *Store) MarkFeedRead(ctx context.Context, userID, feedID int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO read_items (user_id, item_id)
		SELECT ?, i.id FROM items i
		JOIN subscriptions s ON s.feed_id = i.feed_id AND s.user_id = ?
		WHERE i.feed_id = ?`, userID, userID, feedID)
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
			UPDATE feeds SET title = ?, site_url = ?, etag = ?, last_modified = ?,
			       last_error = '', failures = 0, next_fetch_at = ? WHERE id = ?`,
			r.Title, r.SiteURL, r.ETag, r.LastModified, next.Unix(), r.FeedID)
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
