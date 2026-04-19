-- +goose Up
CREATE TABLE delivery_attempts (
    id                bigserial    PRIMARY KEY,
    notification_id   uuid         NOT NULL REFERENCES notifications(id),
    attempt_number    int          NOT NULL,
    status            text         NOT NULL,
    error_reason      text,
    provider_response text,
    attempted_at      timestamptz  NOT NULL DEFAULT now()
);

CREATE INDEX idx_delivery_attempts_notification_attempt
    ON delivery_attempts (notification_id, attempt_number);

-- +goose Down
DROP TABLE IF EXISTS delivery_attempts;
