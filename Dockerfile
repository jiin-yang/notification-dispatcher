# syntax=docker/dockerfile:1.7

# ---- builder ----------------------------------------------------------------
FROM golang:1.26-alpine AS builder

ARG SERVICE
RUN test -n "$SERVICE" || (echo "SERVICE build-arg is required (api|worker)" && false)

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${SERVICE}

# ---- runtime ----------------------------------------------------------------
FROM alpine:3.23

RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S app && adduser -S -G app app

WORKDIR /app

COPY --from=builder /out/app /app/app

USER app:app
ENTRYPOINT ["/app/app"]
