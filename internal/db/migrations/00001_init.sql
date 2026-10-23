-- +goose Up
CREATE TABLE users (
    id         INTEGER PRIMARY KEY,
    email      TEXT NOT NULL UNIQUE,
    created_at INTEGER NOT NULL
);

-- Feeds are shared between users so each URL is fetched once.
CREATE TABLE feeds (
    id              INTEGER PRIMARY KEY,
    url             TEXT NOT NULL UNIQUE,
    title           TEXT NOT NULL DEFAULT '',
    site_url        TEXT NOT NULL DEFAULT '',
    etag            TEXT NOT NULL DEFAULT '',
    last_modified   TEXT NOT NULL DEFAULT '',
    next_fetch_at   INTEGER NOT NULL,
    last_error      TEXT NOT NULL DEFAULT '',
    failures        INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX feeds_next_fetch_at ON feeds (next_fetch_at);

CREATE TABLE subscriptions (
    user_id INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    feed_id INTEGER NOT NULL REFERENCES feeds (id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, feed_id)
);
CREATE INDEX subscriptions_feed_id ON subscriptions (feed_id);

CREATE TABLE items (
    id           INTEGER PRIMARY KEY,
    feed_id      INTEGER NOT NULL REFERENCES feeds (id) ON DELETE CASCADE,
    guid         TEXT NOT NULL,
    url          TEXT NOT NULL DEFAULT '',
    title        TEXT NOT NULL DEFAULT '',
    summary      TEXT NOT NULL DEFAULT '',
    published_at INTEGER NOT NULL,
    UNIQUE (feed_id, guid)
);
CREATE INDEX items_feed_published ON items (feed_id, published_at DESC);

CREATE TABLE read_items (
    user_id INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    item_id INTEGER NOT NULL REFERENCES items (id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, item_id)
);

-- +goose Down
DROP TABLE read_items;
DROP TABLE items;
DROP TABLE subscriptions;
DROP TABLE feeds;
DROP TABLE users;
