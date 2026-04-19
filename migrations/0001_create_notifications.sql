-- +goose Up
-- +goose StatementBegin
CREATE TABLE notifications (
    id              UUID PRIMARY KEY,
    recipient       TEXT        NOT NULL,
    channel         TEXT        NOT NULL,
    content         TEXT        NOT NULL,
    priority        TEXT        NOT NULL DEFAULT 'normal',
    status          TEXT        NOT NULL DEFAULT 'pending',
    correlation_id  UUID        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_notifications_status_created_at ON notifications (status, created_at);
CREATE INDEX idx_notifications_channel            ON notifications (channel);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS notifications;
-- +goose StatementEnd
