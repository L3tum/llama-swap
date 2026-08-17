-- +goose Up
ALTER TABLE activity ADD COLUMN speed_approx INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE activity DROP COLUMN speed_approx;
