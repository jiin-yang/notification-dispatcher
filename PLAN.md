# Notification Dispatcher — Uygulama Planı

Scalable, event-driven notification sistemi. Go + RabbitMQ + PostgreSQL üzerine
kurulu; SMS / Email / Push kanallarına mesaj dağıtır.

> **İlke:** Her faz sonunda uçtan uca çalışabilir, demo edilebilir bir sistem
> olacak. Basit başla, gerektikçe katmanla.

---

## 1. Kararlar Özeti

| Karar | Seçim | Gerekçe |
|---|---|---|
| Outbox pattern | **Hayır** (Faz 4'te revisit) | Basitlik; publisher confirms yeterli kabul edildi |
| API authentication | **Yok** | Kapsam dışı |
| Rate limit scope | **In-process** (tek instance) | Cluster-wide için Redis gerekli, şimdilik gereksiz |
| Rate limit noktası | **API ingest** — token bucket middleware | 100 msg/s per channel, aşan istek 429 |
| Priority mekanizması | **Per-queue** (`{channel}.{priority}`) | Tahmin edilebilir; her seviyeye ayrı prefetch |
| Retry parametreleri | Tüm kanallar aynı (ileride farklılaşabilir) | Basitlik |
| Cancellation | Sadece `pending` statüsündekiler | In-flight recall karmaşıklığı yok |
| Tenancy | **Single tenant** | Kapsam dışı |
| Provider | **HTTP webhook** (webhook.site) — tüm kanallar için aynı | Gerçek 3rd party entegrasyonu kapsam dışı; webhook, HTTP call semantiğini simüle eder |
| Webhook başarı kriteri | Sadece **HTTP status code** (2xx=OK) — response body yok sayılır | Kullanıcı gereksinimi |
| Observability | `/health`, `/metrics` (Prometheus), correlation_id | `/stats` gerekli değil |

### Kütüphane Seçimleri

| Katman | Kütüphane |
|---|---|
| HTTP router | `go-chi/chi/v5` |
| AMQP client | `rabbitmq/amqp091-go` |
| PostgreSQL | `jackc/pgx/v5` + `pgxpool` |
| Migrations | `pressly/goose/v3` |
| Logger | `log/slog` (stdlib) |
| Config | `caarlos0/env/v11` |
| Rate limiter | `golang.org/x/time/rate` |
| Circuit breaker | `sony/gobreaker` (Faz 4) |
| Metrics | `prometheus/client_golang` (Faz 5) |
| Testing | `stretchr/testify` |

### Proje Yapısı

```
/cmd
  /api          -> HTTP API binary
  /worker       -> consumer worker binary
/internal
  /domain       -> entity, value object, enum (Notification, Channel, Priority, Status)
  /app          -> use case / service (CreateNotification, ListNotifications, ...)
  /adapter
    /http       -> handler, middleware, DTO
    /postgres   -> repository implementations
    /rabbitmq   -> publisher, consumer, topology setup
    /provider   -> mock email/sms/push adapters
  /config       -> env parsing
  /platform
    /logger     -> slog setup
    /ratelimit  -> token bucket helper
/migrations     -> goose SQL files
/docs           -> ek dokümanlar
Makefile
docker-compose.yml  (devops-engineer tarafından)
go.mod
```

---

## 2. Faz 1 — Minimal Dikey Dilim

**Hedef:** API → RabbitMQ → worker → webhook → DB status zincirini uçtan uca
çalıştırmak. Tüm kanallar (sms/email/push) kabul edilir ama tek kuyruk üzerinden
akar; priority sabit `normal` kabul edilir. Webhook provider webhook.site'a
HTTP POST atar, status code 2xx ise delivered sayar.

### API Sözleşmesi

**İstek** — `POST /notifications`
```json
{
  "to": "+905551234567",
  "channel": "sms",
  "content": "Your message"
}
```
- `to` — zorunlu (E.164 format veya email, validation kanala göre)
- `channel` — zorunlu, enum: `sms` | `email` | `push`
- `content` — zorunlu, max 500 karakter
- `priority` — opsiyonel, Faz 1'de kabul edilir ama yok sayılır (default `normal`)

**Yanıt** — `202 Accepted`
```json
{
  "messageId": "d490fd94-60b3-4ead-9543-642bfc076f58",
  "status": "accepted",
  "timestamp": "2026-04-19T12:00:00Z"
}
```
- `messageId` — camelCase (dış sözleşme)
- `status` — API accept onayı, notification delivery status'u DEĞİL
- `timestamp` — ISO8601 UTC

### Webhook Sözleşmesi

Worker delivery sırasında `WEBHOOK_URL`'e `POST` atar:
```json
{
  "notification_id": "d490fd94-60b3-4ead-9543-642bfc076f58",
  "recipient": "+905551234567",
  "channel": "sms",
  "content": "Smoke test",
  "correlation_id": "31f4cea6-68c6-4cfb-88d2-7ad36a5f1e16"
}
```
- Alan isimleri snake_case (iç sözleşme)
- **Başarı:** HTTP 2xx → `delivered`. Response body yok sayılır.
- **Başarısızlık:** non-2xx veya connection error → `failed` (Faz 1'de requeue=false; Faz 4'te retry)

### Kapsam

- Tek notification oluşturma (`POST /notifications`)
- Status sorgulama (`GET /notifications/{messageId}`)
- Health endpoint (`GET /health`)
- Content validation (zorunlu alanlar, karakter limiti, recipient format)
- Webhook provider (HTTP POST → status check)
- Structured logging + correlation_id

### Kapsam Dışı

Batch, list, cancel, priority ayrımı, rate limit, retry, metrics, auth,
idempotency, per-channel queue ayrımı.

### Taskler

#### 1.1 Proje İskelet
- [ ] `go.mod` init (module: `github.com/<owner>/notification-dispatcher`)
- [ ] Dizin yapısı oluştur (`cmd/`, `internal/...`, `migrations/`)
- [ ] `.editorconfig`, `.golangci.yml` (basit — `errcheck`, `govet`, `staticcheck`)
- [ ] `Makefile`: `build`, `test`, `lint`, `run-api`, `run-worker`
- [ ] Devops handoff — `docker-compose.yml` (rabbitmq:3.13-management + postgres:16) ve `Makefile` (`dev-up`, `dev-down`)

#### 1.2 Domain Modeli
- [ ] `internal/domain/notification.go`: `Notification` struct (id, recipient, channel, content, priority, status, correlation_id, created_at, updated_at)
- [ ] Enum tipleri: `Channel` (email/sms/push), `Priority` (high/normal/low), `Status` (pending/delivered/failed)
- [ ] Validation: `to` boş değil, `content` 1–500 karakter, `channel` enum içinde, `to` kanala göre format (sms → E.164 regex, email → RFC 5322 basit regex, push → device token string)

#### 1.3 PostgreSQL Katmanı
- [ ] `migrations/0001_create_notifications.sql` (goose format)
  - Kolonlar: `id` (uuid PK), `recipient` (text), `channel` (text), `content` (text), `priority` (text, default `normal`), `status` (text, default `pending`), `correlation_id` (uuid), `created_at`, `updated_at`
  - Indexler: `(status, created_at)`, `(channel)`
- [ ] `internal/adapter/postgres/notification_repository.go`: `Insert(ctx, n) error`, `GetByID(ctx, id) (Notification, error)`, `UpdateStatus(ctx, id, status) error`
- [ ] `internal/platform/db.go`: `pgxpool.New` + ping

#### 1.4 RabbitMQ Katmanı
- [ ] `internal/adapter/rabbitmq/connection.go`: connection + channel lifecycle
- [ ] `internal/adapter/rabbitmq/topology.go`: tek exchange (`notifications.topic`), **tek queue** (`notifications.default`), binding key `#` (tüm mesajlar — Faz 3'te per-channel ayrışacak)
- [ ] `internal/adapter/rabbitmq/publisher.go`: publisher confirms mode, `Publish(ctx, routingKey, headers, payload) error`. Header'larda: `correlation_id`, `notification_id`
- [ ] `internal/adapter/rabbitmq/consumer.go`: manual ack, handler fonksiyon alır

#### 1.5 Webhook Provider
- [ ] `internal/domain/provider.go`: `Provider` interface → `Send(ctx, notification) error`
- [ ] `internal/adapter/provider/webhook.go`:
  - HTTP POST to `WEBHOOK_URL`
  - Body: `{notification_id, recipient, channel, content, correlation_id}` (snake_case)
  - Timeout: 10s
  - Başarı: response status `2xx`. Body okunmaz/ignore edilir.
  - Başarısızlık: non-2xx → `ErrDeliveryFailed`, network error → wrap error
  - `Content-Type: application/json` header

#### 1.6 API Servisi
- [ ] `cmd/api/main.go`: config yükle, logger init, DB ve RMQ connection, chi router, graceful shutdown
- [ ] `internal/adapter/http/dto.go`: request/response DTO'lar
  - Request: `{to, channel, content, priority?}`
  - Response: `{messageId, status, timestamp}` — **camelCase** (dış sözleşme)
- [ ] `internal/adapter/http/handler_notification.go`:
  - `POST /notifications` → DTO parse → validate → correlation_id üret (veya header'dan al) → DB insert (status=pending) → RMQ publish (headers: correlation_id, notification_id) → 202 `{messageId, status:"accepted", timestamp: time.Now().UTC().Format(RFC3339)}`
  - `GET /notifications/{messageId}` → repo lookup → 200 JSON veya 404
- [ ] `internal/adapter/http/handler_health.go`: DB ping + RMQ connection state
- [ ] Middleware: correlation_id (request header `X-Correlation-ID` varsa kullan, yoksa uuid üret, response header'a ekle), slog request logger

#### 1.7 Worker Servisi
- [ ] `cmd/worker/main.go`: config, logger, DB, RMQ setup, consumer start, graceful shutdown (`signal.NotifyContext`)
- [ ] `internal/app/deliver.go`:
  - Mesajı parse et, header'dan correlation_id al
  - Webhook provider'a gönder
  - Başarılı: `UpdateStatus(delivered)` + `ack`
  - Başarısız: `UpdateStatus(failed)` + `nack(requeue=false)` (Faz 4'te retry gelecek)
- [ ] Log her adımda: `correlation_id`, `notification_id`, `channel`, `recipient` (maskeli — son 4 karakter gibi)

#### 1.8 Config
- [ ] `internal/config/config.go`: env struct
  - `HTTP_PORT` (default `8080`)
  - `DB_DSN` (zorunlu)
  - `RMQ_URL` (zorunlu)
  - `WEBHOOK_URL` (zorunlu) — örn. `https://webhook.site/21932283-4eb3-4b39-95d1-33f471adfab7`
  - `WEBHOOK_TIMEOUT_SECONDS` (default `10`)
  - `LOG_LEVEL` (default `info`)
- [ ] `.env.example` dosyası — yukarıdaki tüm env'ler örnekleriyle

#### 1.9 Testler
- [ ] Domain validation unit test (geçerli/geçersiz `to`, `channel`, `content`)
- [ ] Webhook provider unit test: `httptest.Server` ile 200/500/timeout senaryoları
- [ ] Repository integration test (testcontainers-go veya `TEST_DB_DSN` varsa çalıştır)
- [ ] API handler test (httptest + in-memory repo mock)
- [ ] E2E smoke: gerçek webhook.site URL'ine tek request at, dashboard'da görülsün (manuel)

### Faz 1 Demo

```bash
make dev-up
make migrate-up
make run-api &
make run-worker &

curl -X POST localhost:8080/notifications \
  -H "Content-Type: application/json" \
  -H "X-Correlation-ID: 31f4cea6-68c6-4cfb-88d2-7ad36a5f1e16" \
  -d '{"to":"+905551234567","channel":"sms","content":"Smoke test"}'
# -> 202 {"messageId":"d490fd94-...","status":"accepted","timestamp":"2026-04-19T12:00:00Z"}

# worker logu:
# level=INFO msg="webhook delivered" correlation_id=31f4... notification_id=d490... channel=sms status=200

# webhook.site dashboard'unda request görülür:
# POST body: {"notification_id":"d490...","recipient":"+905551234567","channel":"sms","content":"Smoke test","correlation_id":"31f4..."}

curl localhost:8080/notifications/d490fd94-60b3-4ead-9543-642bfc076f58
# -> 200 {"messageId":"d490...","status":"delivered","channel":"sms","recipient":"+905551234567",...}

curl localhost:8080/health
# -> 200 {"status":"ok"}
```

---

## 3. Faz 2 — Tam API Yüzeyi

**Hedef:** Tüm Notification Management API'sini tamamlamak (batch, list, cancel,
batch query, idempotency) — hâlâ email/normal üzerinde.

### Kapsam

- `POST /notifications/batch` — max 1000
- `GET /notifications/batch/{batch_id}` — batch özeti
- `GET /notifications` — filtre (status, channel, date_from, date_to) + pagination
- `DELETE /notifications/{id}` — sadece `pending`
- `Idempotency-Key` header desteği

### Taskler

#### 2.1 DB Değişiklikleri
- [ ] `migrations/0002_create_batches.sql`: `batches` tablosu (id, total_count, pending_count, delivered_count, failed_count, cancelled_count, created_at)
- [ ] `migrations/0003_add_batch_id_and_idempotency.sql`: `notifications.batch_id` (nullable FK), `notifications.idempotency_key` (unique nullable)
- [ ] Index: `(status, created_at)`, `(channel, status)`, `batch_id`

#### 2.2 Repository
- [ ] `InsertBatch(notifications []Notification)` — tek transaction, bulk insert
- [ ] `GetBatchSummary(batchID)` — counter'ları dinamik sorgu ile getir (basit)
- [ ] `List(filter, page, limit)` — cursor-based veya offset-based (offset daha basit, onunla başla)
- [ ] `UpdateStatusIfPending(id, newStatus)` — cancel için conditional update
- [ ] `GetByIdempotencyKey(key)` — var mı kontrolü

#### 2.3 API Handler
- [ ] `POST /notifications/batch` — max 1000 validation, bulk insert, her biri için publish
- [ ] `GET /notifications/batch/{id}` — özet döner
- [ ] `GET /notifications` — query param parse, filtre validation, pagination response (total, page, limit, items)
- [ ] `DELETE /notifications/{id}` — sadece pending → status=cancelled, değilse 409
- [ ] Idempotency middleware: `Idempotency-Key` header varsa önce lookup, varsa eski sonucu döner

#### 2.4 Testler
- [ ] Batch insert transactional rollback testi
- [ ] Idempotency — aynı key ile iki istek, tek kayıt
- [ ] Cancel — pending cancel edilir, delivered cancel edilemez (409)
- [ ] List filter kombinasyonları

### Faz 2 Demo

```bash
curl -X POST localhost:8080/notifications/batch -d '{"notifications":[... 1000 item ...]}'
# -> 202 {"batch_id":"<uuid>","accepted":1000}

curl localhost:8080/notifications/batch/<batch_id>
# -> {"total":1000,"pending":450,"delivered":548,"failed":2}

curl "localhost:8080/notifications?status=delivered&channel=email&page=1&limit=50"
# -> sayfalı liste

curl -X DELETE localhost:8080/notifications/<pending_id>
# -> 200 {"status":"cancelled"}

curl -X POST localhost:8080/notifications -H "Idempotency-Key: abc123" -d '{...}'
# 2. istek -> aynı id dönsün
```

---

## 4. Faz 3 — Multi-Channel + Priority + Rate Limit + Idempotent Consumer

**Hedef:** SMS ve Push kanallarını ekle, 3 priority seviyesini queue
topolojisine yansıt, API ingress'inde kanal başına 100 msg/s rate limit uygula,
consumer idempotency'yi pekiştir.

### Kapsam

- SMS + Push mock provider
- 9 queue: `{channel}.{priority}` (email|sms|push × high|normal|low)
- API'de per-channel in-process token bucket (100 req/s, burst: 200)
- Consumer worker'ında dedup tablosu (`processed_messages`)

### Taskler

#### 3.1 Provider Adapter'lar
- [ ] `internal/adapter/provider/sms_mock.go`
- [ ] `internal/adapter/provider/push_mock.go`
- [ ] Channel → Provider routing (map/factory)

#### 3.2 RMQ Topoloji Genişletme
- [ ] `topology.go` 9 queue declare etsin; routing key: `{channel}.{priority}`
- [ ] Worker'da her queue için ayrı consumer goroutine (konfigüre edilebilir worker count, prefetch)
- [ ] API publish routing key hesaplaması: `fmt.Sprintf("%s.%s", n.Channel, n.Priority)`

#### 3.3 Rate Limit Middleware
- [ ] `internal/platform/ratelimit/bucket.go`: `x/time/rate` wrapper, per-channel map
- [ ] HTTP middleware: request body'den channel çıkar (batch için her notification'ı say) → bucket.Allow(n) → false ise 429 `Retry-After` header ile
- [ ] Config: `RATE_LIMIT_PER_SECOND` (default 100), `BURST` (default 200)

#### 3.4 Consumer Idempotency
- [ ] `migrations/0004_create_processed_messages.sql`: `message_id` PK, `processed_at`
- [ ] API publish sırasında message_id = notification_id üret
- [ ] Consumer: handler başında `INSERT INTO processed_messages (...) ON CONFLICT DO NOTHING` → affected rows 0 ise ack ve skip

#### 3.5 Testler
- [ ] Rate limit: 150 req/s gönder, ~50'si 429 olsun
- [ ] Priority: high queue dolu, low queue'ya hiç gitmeyen worker davranışı
- [ ] Consumer idempotency: aynı message 2 kez, provider tek çağrılır

### Faz 3 Demo

```bash
# Karışık batch: 300 email.high + 400 sms.normal + 300 push.low
curl -X POST localhost:8080/notifications/batch -d '{...}'

# Worker logu:
# email.high queue'dan 300 delivered
# sms.normal queue'dan 400 delivered
# push.low queue'dan 300 delivered
# high önce biter (paralel ama prefetch etkili)

# Rate limit testi:
hey -n 1000 -c 50 -q 150 -m POST localhost:8080/notifications -d '{...email...}'
# ~100 req/s kabul, geri kalan 429
```

---

## 5. Faz 4 — Retry, DLQ, Circuit Breaker

**Hedef:** Teslimat başarısızlıklarını kontrollü yönet. Exponential backoff ile
retry, max deneme sonrası DLQ, provider arızasında circuit breaker.

### Retry Stratejisi

```
Queue: email.normal
  ↓ (consumer nack, requeue=false)
DLX: notifications.retry.exchange
  ↓ routing key: email.normal
Queue: email.retry.1  (TTL=30s, DLX=notifications.topic, routing=email.normal)
  ↓ TTL dolar
Queue: email.normal  (tekrar dene, attempt=2)
  ↓ hala fail
email.retry.2  (TTL=2m)
  ↓
email.normal  (attempt=3)
  ↓ hala fail
email.retry.3  (TTL=10m)
  ↓
email.normal  (attempt=4)
  ↓ hala fail
Queue: email.dlq  (kalıcı, manuel inceleme)
```

Attempt sayısı mesaj header'ında (`x-attempt`) taşınır. 3'ten fazla olan DLQ'ya
gider.

### Taskler

#### 4.1 Topoloji Genişletme
- [ ] `notifications.retry.exchange` (topic) + `{channel}.retry.{1,2,3}` queue (TTL 30s/120s/600s)
- [ ] `notifications.dlq.exchange` + `{channel}.dlq` queue (durable, no TTL)
- [ ] Ana queue'lara `x-dead-letter-exchange` argümanı

#### 4.2 Consumer Retry Mantığı
- [ ] Handler hata dönerse attempt header'ı oku → <3 ise retry exchange'e publish (retry level hesapla), orijinal mesajı ack; ==3 ise DLQ'ya publish, ack
- [ ] `delivery_attempts` tablosuna her denemede satır ekle

#### 4.3 Circuit Breaker
- [ ] `sony/gobreaker` provider başına wrap
- [ ] Config: failure threshold, open duration, half-open max calls
- [ ] Breaker `open` iken: mesajı retry exchange'e direkt gönder (hızlı geri çekil)
- [ ] Structured log: breaker state transition'ları

#### 4.4 Admin Endpoints
- [ ] `GET /admin/dlq/{channel}` — DLQ'daki mesaj sayısı (RabbitMQ management API)
- [ ] `POST /admin/dlq/{channel}/replay?limit=N` — N mesajı geri kuyruğa

#### 4.5 Migrations
- [ ] `migrations/0005_create_delivery_attempts.sql`: notification_id, attempt_number, status, error_reason, attempted_at, provider_response

#### 4.6 Testler
- [ ] Mock provider'a configurable failure rate → retry zinciri doğrulansın
- [ ] DLQ replay → mesaj tekrar işlenir
- [ ] Circuit breaker open iken consume hızla retry'a düşer
- [ ] **Outbox revisit kararı:** bu faz sonunda güvenilirlik yeterli mi? Değilse outbox ekle (ayrı iş kartı).

### Faz 4 Demo

```bash
# Mock email provider'ı %60 fail rate'e set et
curl -X POST localhost:8080/admin/mock/email/fail-rate?rate=0.6

curl -X POST localhost:8080/notifications -d '{"recipient":"x@y.com","channel":"email",...}'
# worker logu:
# attempt 1 fail -> retry.1 (30s)
# attempt 2 fail -> retry.2 (2m)
# attempt 3 fail -> retry.3 (10m)
# attempt 4 fail -> DLQ

curl localhost:8080/notifications/<id>
# status=failed, attempts=4

curl -X POST localhost:8080/admin/dlq/email/replay?limit=10
# provider fail rate 0'a çek, mesajlar deliver olsun
```

---

## 6. Faz 5 — Observability + Hardening

**Hedef:** Gözlemlenebilirlik, health check ve graceful shutdown ile
production-ready olgunluk.

### Kapsam

- `/metrics` endpoint (Prometheus format)
- `/health` zenginleştirme (liveness/readiness)
- Queue depth scrape (RabbitMQ management API)
- Graceful shutdown (tüm in-flight işler biter, sonra çıkış)
- Request/response size limit, panic recovery middleware

### Taskler

#### 5.1 Metrics
- [ ] `prometheus/client_golang` entegrasyonu
- [ ] Counter: `notifications_created_total{channel,priority}`, `notifications_delivered_total{channel}`, `notifications_failed_total{channel,reason}`
- [ ] Histogram: `http_request_duration_seconds{handler,method,status}`, `delivery_duration_seconds{channel}`
- [ ] Gauge: `rabbitmq_queue_depth{queue}` (RMQ mgmt API'den periyodik pull), `circuit_breaker_state{provider}`
- [ ] `GET /metrics` → `promhttp.Handler()`

#### 5.2 Health
- [ ] `GET /health/live` — her zaman 200 (süreç çalışıyor mu)
- [ ] `GET /health/ready` — DB ping + RMQ ping, fail → 503

#### 5.3 Logging Zenginleştirme
- [ ] slog attribute'lar: `service`, `version`, `env`, `correlation_id`, `notification_id`, `batch_id`, `attempt`
- [ ] Log level env üzerinden (`LOG_LEVEL=debug|info|warn|error`)
- [ ] Panic recovery middleware → stack trace + 500

#### 5.4 Graceful Shutdown
- [ ] `signal.NotifyContext(SIGINT, SIGTERM)`
- [ ] Sıra: HTTP server.Shutdown → consumer Cancel + drain → outbox relay (varsa) stop → AMQP channel/conn close → pgxpool close
- [ ] Timeout (30s) sonrası force quit

#### 5.5 Hardening
- [ ] `http.MaxBytesReader` (default 1MB, batch için 10MB)
- [ ] Request timeout middleware
- [ ] Input sanitization (recipient format check — email regex, E.164 phone, vb.)

### Faz 5 Demo

```bash
curl localhost:8080/metrics
# HELP notifications_delivered_total ...
# TYPE counter
# notifications_delivered_total{channel="email"} 1523
# rabbitmq_queue_depth{queue="email.normal"} 12

curl localhost:8080/health/ready
# {"status":"ok","postgres":"ok","rabbitmq":"ok"}

kill -TERM $WORKER_PID
# log: "graceful shutdown starting" → "consumers stopped" → "in-flight drained" → "exit"
```

**Devops handoff:** Bu fazda devops-engineer'a `/metrics` endpoint + metric
isim listesi verilip Prometheus scrape config + Grafana dashboard talep edilir.

---

## 7. Faz 6 (Opsiyonel) — Load Test + Tuning

**Hedef:** Günlük milyon mesaj hedefini doğrulamak.

### Taskler
- [ ] `k6` script'i: ramp-up senaryosu (100 → 1000 req/s)
- [ ] pgxpool `MaxConns` tuning
- [ ] RMQ consumer `prefetchCount` tuning
- [ ] Publish batching (relay varsa)
- [ ] `EXPLAIN ANALYZE` raporları, eksik index tespiti
- [ ] Worker yatay ölçekleme testi (compose ile 3 replica)

**Devops handoff:** Kaynak limitli compose + k6 container setup.

---

## 8. Devops İşbirliği Matrisi

| Faz | İstek |
|---|---|
| 1 | `docker-compose.yml` (rabbitmq:3.13-mgmt + postgres:16 + healthcheck). `Makefile` hedefleri: `dev-up`, `dev-down`, `migrate-up`, `migrate-down`, `logs` |
| 2 | Değişiklik yok |
| 3 | Değişiklik yok (in-process rate limit, Redis gereksiz) |
| 4 | Değişiklik yok |
| 5 | `docker-compose.override.yml`: prometheus + grafana. `/metrics` scrape config. Dashboard JSON (queue depth, success rate, latency) |
| 6 | Resource-limited compose + k6 container |

---

## 9. Sonraki Adım

Faz 1 taskleri (bölüm 2) üzerinden tek tek ilerlemek. Her alt bölüm (1.1–1.9)
bitirildikçe commit atılır ve plan üzerinde checkbox işaretlenir.

Başlamadan önce **devops-engineer** ile konuşup Faz 1 için
`docker-compose.yml` + `Makefile` hazırlatılması öneriliyor.
