-- +goose Up
ALTER TABLE subscriptions ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE subscriptions DROP COLUMN pinned;
