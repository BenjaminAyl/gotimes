-- +goose Up
ALTER TABLE feeds ADD COLUMN icon BLOB;
ALTER TABLE feeds ADD COLUMN icon_type TEXT NOT NULL DEFAULT '';
ALTER TABLE feeds ADD COLUMN icon_checked_at INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE feeds DROP COLUMN icon_checked_at;
ALTER TABLE feeds DROP COLUMN icon_type;
ALTER TABLE feeds DROP COLUMN icon;
