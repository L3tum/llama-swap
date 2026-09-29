-- +goose Up
ALTER TABLE activity ADD COLUMN remote_ip TEXT NOT NULL DEFAULT '';
ALTER TABLE activity ADD COLUMN user_agent TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE activity DROP COLUMN remote_ip;
ALTER TABLE activity DROP COLUMN user_agent;
