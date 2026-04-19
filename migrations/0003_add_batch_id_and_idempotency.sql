-- +goose Up
-- +goose StatementBegin
ALTER TABLE notifications
    ADD COLUMN batch_id         UUID NULL REFERENCES batches(id),
    ADD COLUMN idempotency_key  TEXT NULL;

CREATE UNIQUE INDEX idx_notifications_idempotency_key
    ON notifications (idempotency_key)
    WHERE idempotency_key IS NOT NULL;

CREATE INDEX idx_notifications_batch_id
    ON notifications (batch_id);

CREATE INDEX idx_notifications_channel_status
    ON notifications (channel, status);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_notifications_channel_status;
DROP INDEX IF EXISTS idx_notifications_batch_id;
DROP INDEX IF EXISTS idx_notifications_idempotency_key;
ALTER TABLE notifications
    DROP COLUMN IF EXISTS idempotency_key,
    DROP COLUMN IF EXISTS batch_id;
-- +goose StatementEnd
