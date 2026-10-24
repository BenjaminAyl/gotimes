-- +goose Up
CREATE TABLE categories (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    icon       TEXT NOT NULL DEFAULT '',
    pinned     INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    UNIQUE (user_id, name)
);

-- A feed sits in at most one category per user; deleting the category un-files its feeds.
ALTER TABLE subscriptions ADD COLUMN category_id INTEGER REFERENCES categories (id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE subscriptions DROP COLUMN category_id;
DROP TABLE categories;
