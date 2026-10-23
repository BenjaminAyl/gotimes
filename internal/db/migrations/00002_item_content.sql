-- +goose Up
ALTER TABLE items ADD COLUMN author  TEXT NOT NULL DEFAULT '';
ALTER TABLE items ADD COLUMN content TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE items DROP COLUMN content;
ALTER TABLE items DROP COLUMN author;
