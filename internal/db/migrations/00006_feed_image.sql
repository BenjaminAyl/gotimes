-- +goose Up
ALTER TABLE feeds ADD COLUMN image_url TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE feeds DROP COLUMN image_url;
