# Wallet & Ledger Service

A REST API backend for a digital wallet: **top-up**, **wallet-to-wallet transfer**, **withdraw**, and **transaction history**, with guaranteed balance consistency, **idempotency**, and a full **double-entry bookkeeping** audit trail.

Built **without a web framework** — only `net/http` (including the Go 1.22+ pattern routing) and `log/slog` from the standard library, plus a database driver, a migration tool, and a metrics client.

```bash
docker compose up -d --build   # PostgreSQL + migrations + service, one command
./scripts/e2e.sh               # full end-to-end scenario with curl
```

---

## 1. Features

| Capability | Implementation |
|---|---|
| Consistent balances | Database transaction + `SELECT ... FOR UPDATE` (row-level lock), plus `CHECK (balance >= 0)` in the database |
| Audit trail | `transactions` + `ledger_entries` (debit/credit), `balance_after` per entry, `transaction_totals` reconciliation view |
| Double-entry | Every money movement has a debit and a credit side; a `DEFERRABLE` constraint trigger rejects unbalanced journals |
| Idempotency | `idempotency_keys` table + `UNIQUE(key)` + stored response for replay |
| Deadlock avoidance | Wallet locks are always taken in sorted id order (canonical order) |
| Exact money arithmetic | `BIGINT` minor units; never `float64` |
| Consistent errors | RFC 7807 Problem Details from a single centralized mapper |
| Observability | `log/slog` (one structured log line per request) + `/metrics` (Prometheus) + `/healthz` & `/readyz` |
| Graceful shutdown | `signal.NotifyContext` + `http.Server.Shutdown` |
| Stable pagination | Cursor (keyset) on the entry id, never `OFFSET` |

---

## 2. Running the Service

### 2.1 Whole stack (fastest path)

```bash
docker compose up -d --build
docker compose ps                 # wait until wallet-api is "healthy"
curl localhost:8080/healthz
./scripts/e2e.sh                  # runs the 11 end-to-end scenarios
```

`docker compose up` starts PostgreSQL 16, waits until it is healthy, then starts the service with `MIGRATE_ON_START=true` so the schema and the seed (world account) are ready immediately.

### 2.2 Local development

```bash
make db-up                        # start PostgreSQL only
cp .env.example .env              # optional: adjust the configuration
make migrate-up                   # or: MIGRATE_ON_START=true make run
make run                          # serve on :8080
```

### 2.3 Running the tests

```bash
make test                 # unit tests (no database) - 119 test cases
make test-race            # unit tests with the race detector
make test-integration     # integration tests against PostgreSQL (needs make db-up)
make e2e                  # end-to-end curl scenario
make test-all             # everything above
```

Current test suite:

| Layer | Type | Tests | Coverage | Notes |
|---|---|---|---|---|
| `internal/domain` | unit | 23 | 66% | balance rules, double-entry, amount/currency validation |
| `internal/usecase` | unit (in-memory fakes) | 22 | 86% | top-up, transfer, idempotency, rollback, pagination |
| `internal/http/handler` | unit (`httptest`) | 27 | 78% | status codes, problem details, headers, cursor |
| `internal/http/middleware` | unit | 17 | 90% | request-id, recover, auth, rate limit, logging, metrics |
| `pkg/apierror` | unit | 21 | 88% | the whole error -> HTTP status mapping |
| `pkg/cursor` | unit | 9 | 100% | cursor encoding/decoding and rejection of invalid input |
| `internal/repository/postgres` | integration (`-tags=integration`) | 8 | 70% | real SQL, concurrent idempotency and bidirectional transfers |

**119 unit test cases** plus 8 integration tests; all pass with `-race`.

---

## 3. API

Base URL: `http://localhost:8080`. Every endpoint under `/v1` accepts an optional
`X-API-Key` header when `API_KEY` is set.

| Method | Path | Purpose | Success |
|---|---|---|---|
| `POST` | `/v1/wallets` | Create a wallet | `201` |
| `GET` | `/v1/wallets/{id}` | Wallet details and balance | `200` |
| `POST` | `/v1/wallets/{id}/topup` | Top-up (requires `Idempotency-Key`) | `200` |
| `POST` | `/v1/wallets/{id}/withdraw` | Withdraw (requires `Idempotency-Key`) | `200` |
| `POST` | `/v1/wallets/{id}/transfer` | Atomic transfer (requires `Idempotency-Key`) | `200` |
| `GET` | `/v1/wallets/{id}/transactions?cursor=&limit=` | Transaction history (cursor pagination) | `200` |
| `GET` | `/healthz` | Liveness (does not touch the DB) | `200` |
| `GET` | `/readyz` | Readiness (pings the DB) | `200` / `503` |
| `GET` | `/metrics` | Prometheus metrics | `200` |

Money is **always** expressed in minor units (integer). `100000` with currency `IDR` means Rp 1.000,00 at scale 2.

### 3.1 Create a wallet

```bash
curl -X POST localhost:8080/v1/wallets \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: create-wallet-a-0001' \
  -d '{"owner_id":"11111111-1111-4111-8111-111111111111","currency":"IDR"}'
```

```json
{
  "id": "1def0f5a-a623-47f9-8af3-d56f6715aa75",
  "owner_id": "11111111-1111-4111-8111-111111111111",
  "currency": "IDR",
  "status": "active",
  "balance_minor": 0,
  "created_at": "2026-09-15T09:35:46.329149Z",
  "updated_at": "2026-09-15T09:35:46.329149Z"
}
```

### 3.2 Top-up

```bash
curl -X POST localhost:8080/v1/wallets/$WALLET_A/topup \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: topup-demo-0001' \
  -d '{"amount_minor":100000,"reference":"transfer-bank-bca-001"}'
```

```json
{
  "transaction_id": "c14ce048-d7ad-47f0-b239-690e16d9bd3d",
  "type": "topup",
  "status": "posted",
  "wallet_id": "1def0f5a-a623-47f9-8af3-d56f6715aa75",
  "amount_minor": 100000,
  "currency": "IDR",
  "balance_after_minor": 100000,
  "reference": "transfer-bank-bca-001",
  "created_at": "2026-09-15T09:35:58.737132137Z"
}
```

In the database a single top-up produces two ledger rows (debit the world account, credit the user wallet):

```
 id |   type   | direction | amount_minor | balance_after |              wallet
----+----------+-----------+--------------+---------------+----------------------------------
 15 | topup    | credit    |       100000 |        100000 | 1def0f5a-...  (user wallet)
 16 | topup    | debit     |       100000 |       -100000 | 00000000-...  (world account)
```

### 3.3 Transfer

```bash
curl -X POST localhost:8080/v1/wallets/$WALLET_A/transfer \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: transfer-demo-0001' \
  -d '{"to_wallet_id":"'$WALLET_B'","amount_minor":40000,"reference":"invoice-2026-0042"}'
```

### 3.4 Withdrawing more than the balance (409)

```json
{
  "type": "https://api.example.com/problems/insufficient_funds",
  "title": "Insufficient Funds",
  "status": 409,
  "detail": "wallet 1def0f5a-a623-47f9-8af3-d56f6715aa75: balance 100000, required 999999999",
  "instance": "/v1/wallets/1def0f5a-a623-47f9-8af3-d56f6715aa75/withdraw",
  "code": "insufficient_funds",
  "request_id": "c321b51d1f9e6ffac6b994202279c2f3"
}
```

### 3.5 Transaction history

```bash
curl "localhost:8080/v1/wallets/$WALLET_A/transactions?limit=2"
```

```json
{
  "data": [
    {
      "entry_id": 15,
      "transaction_id": "c14ce048-d7ad-47f0-b239-690e16d9bd3d",
      "type": "topup",
      "direction": "credit",
      "amount_minor": 100000,
      "balance_after_minor": 100000,
      "currency": "IDR",
      "reference": "transfer-bank-bca-001",
      "counterparty_wallet_id": "00000000-0000-0000-0000-000000000001",
      "created_at": "2026-09-15T09:35:58.737132Z"
    }
  ],
  "pagination": { "has_more": false, "limit": 2 }
}
```

When `has_more` is `true`, the response also carries a `next_cursor` for the following page:

```bash
curl "localhost:8080/v1/wallets/$WALLET_A/transactions?limit=2&cursor=ZTo0"
```

### 3.6 Status codes and error format

Every error uses **RFC 7807** with the additional `code`, `request_id`, and `errors[]` (per-field validation) members.

| HTTP | `code` | When |
|---|---|---|
| 400 | `bad_request` | Invalid JSON, unknown field, missing `Idempotency-Key` header, non-numeric cursor/limit |
| 401 | `unauthorized` | Wrong or missing `X-API-Key` (when auth is enabled) |
| 403 | `forbidden` | Operation on the system wallet |
| 404 | `not_found` | Wallet or endpoint does not exist |
| 405 | `method_not_allowed` | Wrong method (the response carries an `Allow` header) |
| 409 | `insufficient_funds` | Not enough balance |
| 409 | `wallet_not_active` | Wallet frozen/closed |
| 409 | `idempotency_conflict` | `Idempotency-Key` reused with a different payload |
| 409 | `idempotency_in_progress` | A twin request is still being processed |
| 422 | `validation_error`, `currency_mismatch`, `self_transfer` | Business rule violated |
| 429 | `rate_limited` | Token bucket exhausted |
| 500 | `internal_error` | Bug/unexpected error (no internal detail is leaked) |
| 503 | `database_unavailable` | Database unreachable |
| 504 | `request_timeout` | `REQUEST_TIMEOUT` exceeded |

### 3.7 Idempotency

Money movement endpoints **require** an `Idempotency-Key`. Behaviour:

| Scenario | Result |
|---|---|
| New key | The request is processed and the response is stored |
| Same key, same payload (retry) | The first response is replayed verbatim + `Idempotency-Replayed: true` |
| Same key, different payload | `409 idempotency_conflict` |
| Same key sent concurrently by N clients | Exactly one executes, the rest replay (guaranteed by the PostgreSQL unique index + row lock) |
| First request fails on a business rule (e.g. insufficient balance) | The key is rolled back too, so it can be reused for a new request |

```bash
# first request
curl -X POST localhost:8080/v1/wallets/$WALLET_A/topup \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: topup-demo-0001' \
  -d '{"amount_minor":100000,"reference":"transfer-bank-bca-001"}'

# retry (e.g. after a network timeout): the balance does NOT grow
curl -i -X POST localhost:8080/v1/wallets/$WALLET_A/topup \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: topup-demo-0001' \
  -d '{"amount_minor":100000,"reference":"transfer-bank-bca-001"}'
# => HTTP/1.1 200 OK
#    Idempotency-Replayed: true
```

The full manual collection lives in [`requests.http`](requests.http) (works with the VS Code REST Client and JetBrains IDEs).

---

## 4. Architecture

Clean Architecture with dependencies that always point inwards:

```
cmd/api/main.go                entrypoint + manual wiring (no DI container)
internal/domain/               pure entities & business rules (zero external dependencies)
internal/usecase/              business orchestration; declares the ports (interfaces) it needs
internal/repository/postgres/  SQL adapter (pgx) + Unit of Work
internal/http/handler/         HTTP handlers (parsing & formatting)
internal/http/middleware/      request-id, logging, recover, timeout, auth, rate limit, metrics
internal/platform/             config, logger, database pool, migrations, metrics
pkg/                           reusable helpers: apierror (RFC 7807), response, cursor, reqctx, uuid
migrations/                    version-controlled DDL (embedded in the binary)
```

```
        HTTP request
             │
             v
   middleware chain (request-id → logging → metrics → recover → timeout → rate-limit → auth)
             │
             v
   handler ──► usecase ──► domain          (business rules & double-entry)
                  │
                  v
        repository (SQL + row lock, inside one transaction)
                  │
                  v
             PostgreSQL 16
```

Every decision in detail lives in two companion documents: the long-form design
(ERD, schema decisions, locking and idempotency algorithms, full tech-stack
rationale) in [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md), and the compact
architecture decision records in [`DECISIONS.md`](DECISIONS.md).

### 4.1 The lifecycle of one money movement

```
BEGIN
  1. INSERT idempotency_keys (in_progress)        -- claim; the row lock becomes the critical section
  2. SELECT ... FOR UPDATE wallet (sorted by id)  -- lock balances in canonical order (no deadlock)
  3. validate: wallet status, sufficient funds, same currency
  4. UPDATE wallets SET balance_minor, version
  5. INSERT transactions + 2 ledger_entries
  6. UPDATE idempotency_keys (completed, response_body)
COMMIT  -- the constraint trigger validates SUM(debit) = SUM(credit)
```

---

## 5. Configuration

Everything comes from environment variables (12-factor). Invalid values are reported **all at once** during startup.

| Env | Default | Purpose |
|---|---|---|
| `APP_ENV` | `development` | `development` / `staging` / `production` |
| `HTTP_ADDR` | `:8080` | Listen address |
| `DATABASE_URL` | — | **Required** |
| `DB_MAX_CONNS` / `DB_MIN_CONNS` | `10` / `2` | Pool size |
| `DB_STATEMENT_TIMEOUT` | `5s` | Server-side `statement_timeout` |
| `MIGRATE_ON_START` | `false` | Run migrations at startup |
| `REQUEST_TIMEOUT` | `10s` | Per-request time budget |
| `SHUTDOWN_TIMEOUT` | `15s` | Graceful shutdown budget |
| `MAX_BODY_BYTES` | `1048576` | Body size limit |
| `API_KEY` | *(empty)* | When set, every endpoint requires `X-API-Key` (required in production) |
| `RATE_LIMIT_RPS` / `RATE_LIMIT_BURST` | `50` / `100` | Per-IP token bucket; `0` disables it |
| `IDEMPOTENCY_TTL` | `24h` | How long idempotency keys are kept |
| `SYSTEM_WALLET_ID` | `00000000-...-0001` | World account |

See [`.env.example`](.env.example) for the complete list.

---

## 6. Observability

```bash
curl localhost:8080/healthz      # liveness: the process is alive (no DB access)
curl localhost:8080/readyz       # readiness: pings the database
curl localhost:8080/metrics      # Prometheus
```

Exposed metrics:

| Metric | Type | Labels |
|---|---|---|
| `http_requests_total` | counter | `method`, `route` (ServeMux pattern, not the raw URL), `status` |
| `http_request_duration_seconds` | histogram | `method`, `route` |
| `http_requests_in_flight` | gauge | — |
| `wallet_transactions_total` | counter | `type` (topup/withdrawal/transfer), `result` (success/replayed/error code) |
| `db_pool_connections_total`, `db_pool_connections_idle`, `db_pool_connections_acquired`, `db_pool_max_connections`, `db_pool_acquire_total` | gauge | — |
| `go_*`, `process_*` | provided by client_golang | — |

Logs: one JSON line per request with `request_id`, `method`, `path`, `route`, `status`, `duration`, `bytes`, `client_ip`.

```json
{"time":"2026-09-15T09:35:58Z","level":"INFO","msg":"request completed","service":"wallet-service","request_id":"c321b51d...","method":"POST","path":"/v1/wallets/1def0f5a.../withdraw","route":"POST /v1/wallets/{walletID}/withdraw","status":409,"duration":0.0031,"bytes":264,"client_ip":"127.0.0.1"}
```

---

## 7. Ledger reconciliation

The double-entry invariants can be verified in the database at any time:

```sql
-- every transaction must be balanced
SELECT * FROM transaction_totals WHERE total_debit <> total_credit;

-- the total balance of all wallets must be zero (money moves, it is never created)
SELECT sum(balance_minor) FROM wallets;
```

Both queries also run automatically at the end of `scripts/e2e.sh` and inside every integration test.

---

## 8. Known limitations

This is not a to-do list but a set of deliberate scope decisions (see the rationale in the architecture document):

- Transfers only work between wallets with the same currency (a mismatch returns `422` instead of silently converting).
- There is no per-wallet authorization: `X-API-Key` proves a trusted client, not wallet ownership.
- No message queue / event sourcing / outbox; every operation completes synchronously in one database transaction.
- Rate limiting is per process (not global across replicas).
- No reversal/void endpoint yet (the ledger is append-only).
- `/metrics` is not protected by application auth (restrict it at the network layer).

---

## 9. Troubleshooting

| Symptom | Likely cause |
|---|---|
| `connecting to PostgreSQL failed: connection refused` | PostgreSQL is not ready yet: run `docker compose up -d postgres` and wait for `healthy` |
| `invalid configuration: DATABASE_URL is required` | The environment variable is not exported |
| `the database schema is dirty` | A migration failed midway; fix it manually or `docker compose down -v` for a clean start |
| Port 8080 is already in use | Stop the old process (`pkill -f wallet-api`) or change `HTTP_ADDR` |
| `relation "wallets" does not exist` | Migrations were not applied: `make migrate-up` |
