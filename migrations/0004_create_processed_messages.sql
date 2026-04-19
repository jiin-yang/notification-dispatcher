-- +goose Up
CREATE TABLE processed_messages (
    message_id   UUID        PRIMARY KEY,
    processed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS processed_messages;
