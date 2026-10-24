package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ErrCategoryExists is returned when a user already has a category with that name.
var ErrCategoryExists = errors.New("category already exists")

// Category groups a user's feeds under a name and an icon.
type Category struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Icon   string `json:"icon"`
	Pinned bool   `json:"pinned"`
}

func (s *Store) Categories(ctx context.Context, userID int64) ([]Category, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, icon, pinned FROM categories WHERE user_id = ? ORDER BY pinned DESC, lower(name)`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Category
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.ID, &c.Name, &c.Icon, &c.Pinned); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) CreateCategory(ctx context.Context, userID int64, name, icon string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO categories (user_id, name, icon, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (user_id, name) DO NOTHING`, userID, name, icon, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrCategoryExists
	}
	return res.LastInsertId()
}

// UpdateCategory renames a category and/or changes its icon.
func (s *Store) UpdateCategory(ctx context.Context, userID, id int64, name, icon string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE categories SET name = ?, icon = ?
		 WHERE id = ? AND user_id = ? AND NOT EXISTS
		   (SELECT 1 FROM categories o WHERE o.user_id = ? AND o.name = ? AND o.id != ?)`,
		name, icon, id, userID, userID, name, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrCategoryExists
	}
	return nil
}

func (s *Store) DeleteCategory(ctx context.Context, userID, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM categories WHERE id = ? AND user_id = ?`, id, userID)
	return err
}

// TogglePinCategory flips whether the category is pinned for this user.
func (s *Store) TogglePinCategory(ctx context.Context, userID, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE categories SET pinned = 1 - pinned WHERE id = ? AND user_id = ?`, id, userID)
	return err
}

// MarkCategoryRead marks every item in the category's feeds read.
func (s *Store) MarkCategoryRead(ctx context.Context, userID, id int64) error {
	return s.markRead(ctx, userID, "s.category_id", id)
}

// SetFeedCategory files a subscribed feed under a category; categoryID 0 un-files it.
// It returns sql.ErrNoRows if the feed or category isn't the user's.
func (s *Store) SetFeedCategory(ctx context.Context, userID, feedID, categoryID int64) error {
	var cat any
	if categoryID != 0 {
		var one int
		if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM categories WHERE id = ? AND user_id = ?`, categoryID, userID).Scan(&one); err != nil {
			return err
		}
		cat = categoryID
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE subscriptions SET category_id = ? WHERE user_id = ? AND feed_id = ?`, cat, userID, feedID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
