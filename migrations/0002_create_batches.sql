-- +goose Up
-- +goose StatementBegin
CREATE TABLE batches (
    id          UUID        PRIMARY KEY,
    total_count INT         NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS batches;
-- +goose StatementEnd
