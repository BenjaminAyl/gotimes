-- +goose Up
-- Per-user overrides on a subscription: feeds themselves are shared between users.
ALTER TABLE subscriptions ADD COLUMN custom_title TEXT NOT NULL DEFAULT '';
ALTER TABLE subscriptions ADD COLUMN custom_icon BLOB;
ALTER TABLE subscriptions ADD COLUMN custom_icon_type TEXT NOT NULL DEFAULT '';
ALTER TABLE subscriptions ADD COLUMN custom_icon_v INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE subscriptions DROP COLUMN custom_icon_v;
ALTER TABLE subscriptions DROP COLUMN custom_icon_type;
ALTER TABLE subscriptions DROP COLUMN custom_icon;
ALTER TABLE subscriptions DROP COLUMN custom_title;
