# Architecture — Wallet & Ledger Service

This document is the output of **Stage (a)**: the architectural decisions and the
database schema. The condensed ADR version lives in `DECISIONS.md`; this file is
the full-length version.

Case study: **Wallet & Ledger Service** — a digital wallet with top-up, transfer,
withdraw, and transaction history (ledger) that guarantees balance consistency,
idempotency, and a double-entry bookkeeping audit trail.

---

## 1. Principles & Constraints

| Aspect | Decision |
|---|---|
| Language | Go 1.23+ (build and tests verified on Go 1.26) |
| HTTP | `net/http` standard library only — `http.ServeMux` (Go 1.22+ pattern routing) |
| Web framework | **None** (no Gin/Fiber/Echo/Chi/Gorilla) |
| Database | PostgreSQL 16 |
| DB driver | `pgx/v5` (native protocol, connection pool via `pgxpool`) |
| ORM | **None** — explicit SQL |
| Architecture | Four-layer Clean Architecture, dependencies point inwards |
| Money | `BIGINT` minor units (integer); **never** floats |
| Consistency | Database transaction + row-level lock (`SELECT ... FOR UPDATE`) |

Deliberate non-goals (see §11): message queue, event sourcing, multi-currency
transactions, and per-wallet authorization.

---

## 2. Folder Structure

```
cmd/api/main.go              entrypoint: read config, wire dependencies, run the server
internal/
  domain/                    entities + pure business rules (zero external dependencies)
  usecase/                   application logic; declares the PORTs (interfaces) it needs
  repository/postgres/       adapter: implements the usecase ports with pgx SQL
  http/handler/              adapter: parse requests, call usecases, format responses
  http/middleware/           request-id, logging, recover, auth, rate limit, metrics
  platform/                  infrastructure: config, database pool, logger, migrate, metrics
pkg/
  apierror/                  RFC 7807 Problem Details + error -> HTTP status mapper
  response/                  JSON writer + pagination envelope
  cursor/                    opaque cursor pagination encode/decode (base64url)
  reqctx/                    context helpers: request id, logger, principal
migrations/                  golang-migrate SQL files (embedded via embed.go at the root)
```

### Dependency rule (arrow = allowed import)

```
        ┌──────────────────────────────────────────────┐
        │                  cmd/api                     │  manual wiring
        └───────┬───────────────┬──────────────┬───────┘
                │               │              │
                v               v              v
        ┌───────────────┐ ┌───────────┐ ┌──────────────────┐
        │ http/handler  │ │ repository│ │    platform      │
        │ http/mw       │ │ /postgres │ │ (config, db, log)│
        └───────┬───────┘ └─────┬─────┘ └────────┬─────────┘
                │               │                │
                v               v                │
        ┌───────────────────────────────┐        │
        │           usecase             │        │  implements the ports
        │   (declares the interfaces)   │<───────┘
        └───────────────┬───────────────┘
                        v
        ┌───────────────────────────────┐
        │            domain             │  knows nothing about DB, HTTP, or JSON
        └───────────────────────────────┘
```

* `domain` imports **nothing** from any other `internal/*` package.
* `usecase` only knows `domain` plus the interfaces it declares itself
  (`WalletRepository`, `LedgerRepository`, `IdempotencyRepository`, `UnitOfWork`).
  That is what makes the usecase fully testable with in-memory fakes.
* `repository/postgres` and `http/handler` are adapters that depend on the
  usecase interfaces (dependency inversion).
* Wiring is explicit in `cmd/api/main.go` — no DI container, so the dependency
  graph fits on one screen.

---

## 3. Database Schema

### 3.1 ERD

```
   ┌───────────────────────┐                       ┌──────────────────────────┐
   │       wallets         │                       │      transactions        │
   ├───────────────────────┤                       ├──────────────────────────┤
   │ id            UUID PK │                       │ id            UUID PK    │
   │ owner_id      UUID    │                       │ type          topup|     │
   │ currency      CHAR(3) │                       │               withdraw|  │
   │ status        enum    │                       │               transfer   │
   │ is_system     bool    │                       │ status, reference        │
   │ balance_minor BIGINT  │◄──┐                   │ idempotency_key (uniq)   │
   │ version       BIGINT  │   │                   │ metadata      JSONB      │
   │ created/updated_at    │   │                   │ created_at               │
   └───────────────────────┘   │                   └────────────┬─────────────┘
                               │                                │ 1
                               │                                │
                               │   ┌────────────────────────────┴─────────────┐
                               │   │            ledger_entries                │
                               │   ├──────────────────────────────────────────┤
                               └───┤ id            BIGINT IDENTITY PK (cursor)│
                                   │ transaction_id UUID FK                   │
                                   │ wallet_id      UUID FK                   │
                                   │ direction      debit|credit              │
                                   │ amount_minor   BIGINT > 0                │
                                   │ balance_after  BIGINT  (audit snapshot)  │
                                   │ currency, created_at                     │
                                   └──────────────────────────────────────────┘

   ┌──────────────────────────────┐
   │       idempotency_keys       │   key TEXT PK  (the row lock = anti-retry lock)
   ├──────────────────────────────┤
   │ key PK, endpoint, request_hash, status (in_progress|completed),
   │ status_code, response_body JSONB, resource_id, expires_at
   └──────────────────────────────┘
```

### 3.2 Schema decisions and their rationale

| Decision | Why | Trade-off |
|---|---|---|
| Balance as `BIGINT balance_minor` | Integers are exact; IEEE-754 floats cannot represent money values exactly and their error accumulates | Requires conversion discipline at the API boundary (the API takes minor units directly, so there is no conversion at all) |
| `balance_minor` cached on `wallets` | Reading a balance becomes a single row lookup instead of `SUM`-ing the whole history on every request | Two sources of data → it must be updated in the same DB transaction; a reconciliation view/query is provided |
| `ledger_entries` as the source of truth | Immutable audit trail; any balance can be reconstructed from the entries | History grows → partitioning/archiving is needed at large scale |
| `balance_after` stored per entry | Proves the balance snapshot at a point in time (reconciliation & disputes) | Redundant data, but small compared to the audit value |
| `CHECK (is_system OR balance_minor >= 0)` | Makes "balances cannot go negative" a database invariant instead of only an application check (defense in depth) | Requires the system wallet (world account) concept so double-entry stays balanced |
| Explicit `direction` debit/credit | The ledger can be audited without guessing the sign | Cannot be reused for asset vs liability accounts with a different convention |
| Money movement needs a counterparty account | Without one, top-up/withdraw would be single-sided and debit ≠ credit | The system wallet balance goes negative — intended, and it must be understood during audits |
| `ledger_entries.id` BIGINT identity as the cursor | Monotonic and unique → stable pagination without `OFFSET` (no drift when rows are inserted) | The cursor leaks transaction volume; mitigated by base64url encoding |
| `DEFERRABLE INITIALLY DEFERRED` constraint trigger | Validates `debit = credit` per transaction **at COMMIT**, so writing two rows at once stays valid while an unbalanced ledger is impossible to persist | Small commit overhead, and single-sided transactions become impossible (which is the point) |
| FK `ON DELETE RESTRICT` to wallet | The ledger must not lose its audit trail because a wallet was deleted | Wallets can never be truly deleted → use `status = closed` (soft) |
| No unique `(owner_id, currency)` | One user may have more than one wallet in the same currency (e.g. main wallet and savings wallet) | Duplicate wallets are not prevented by the database; can be added as a business rule later |
| `idempotency_keys` separate from `transactions` | Applies to every mutating endpoint, not only the ones creating a transaction; it can store any response for replay | One extra table plus a job to clean up `expires_at` |

### 3.3 Business invariants (enforced by the DB and the usecase)

1. `balance_minor >= 0` for every non-system wallet.
2. For every `transaction_id`: `SUM(debit) == SUM(credit)` and at least 2 entries.
3. Every entry has `amount_minor > 0` (direction, not sign, encodes the side).
4. Source and destination wallet must share the same currency.
5. A single `idempotency_key` never executes the business effect twice.
6. Every balance mutation always comes with ledger entries in the same commit.

---

## 4. Transaction Flow & Locking Strategy

Every mutating operation follows the same pattern inside **one database transaction**:

```
1. INSERT idempotency_keys (status=in_progress)      ← claim idempotency (row lock)
2. SELECT wallet ... FOR UPDATE  (sorted by id)      ← lock balances
3. Validate business rules (wallet status, funds, currency)
4. UPDATE wallets SET balance_minor = balance_minor ± x, version = version + 1
5. INSERT transactions + 2 ledger_entries
6. UPDATE idempotency_keys (status=completed, status_code, response_body)
COMMIT  → the deferred trigger validates debit = credit
```

### Why `SELECT ... FOR UPDATE` (a DB-level lock) instead of `sync.Mutex`

`sync.Mutex` only locks **within one process**. This service is deployed as
containers that scale to multiple replicas, so two requests for the same wallet
can be served by different processes. The lock must live in the shared resource,
which is the PostgreSQL row. Bonus: the lock is released automatically on commit
or rollback (including a crash), so there is no permanent deadlock.

### Why not optimistic locking (`UPDATE ... WHERE version = $n`)

Optimistic locking forces a retry loop in the application, and top-up/transfer
patterns on the same wallet do conflict often for popular wallets. A pessimistic
row lock simply makes the second request wait (blocking) with predictable latency
and no extra retry logic.

### Why not `SERIALIZABLE`

`SERIALIZABLE` guarantees consistency without explicit locks, but produces random
`40001 serialization_failure` errors that the application **must** retry — more
complexity and higher latency for a case that one row lock already solves.
`READ COMMITTED` + `FOR UPDATE` provides the same guarantee for our access pattern.

### Deadlock avoidance in transfers

A transfer touches two wallets. Two opposite transfers (A→B and B→A) could
naively wait on each other (the classic ABBA deadlock). Therefore:

```go
// the usecase always requests locks in sorted (canonical) id order
for _, id := range sorted(ids) {
    SELECT ... FROM wallets WHERE id = $1 FOR UPDATE
}
```

Locks are acquired in a deterministic order, so no wait cycle can form.
PostgreSQL's `deadlock_timeout` remains as a last-resort safety net, but we do not
rely on it.

### Why not an application-level queue / single writer

A single writer (via a channel or a message queue) does remove the race, but it
becomes a bottleneck and a single point of failure, and it adds infrastructure
(a broker) that this scale does not need. See §11.

---

## 5. Idempotency Strategy

Problem: clients retry (network timeouts, users double-clicking) and the second
request could be executed twice → a double top-up.

Mechanism: the `Idempotency-Key` header is required on every mutating endpoint.
The server stores the result of the first request and replays the same response
for that key.

```
                ┌──────────────────────────────────────────────┐
   request ───► │ INSERT INTO idempotency_keys (key, hash,     │
                │        status='in_progress', expires_at)     │
                └──────────────┬───────────────┬───────────────┘
                       success │               │  unique violation
                               v               v
                    run business logic   SELECT ... FOR UPDATE (wait for the first tx)
                    store the response   ├─ same hash  & completed → replay + header
                    status=completed     ├─ different hash         → 409 idempotency_conflict
                                         └─ still in_progress      → 409 idempotency_in_progress
```

Key points:

* **`key` is the PRIMARY KEY**, so the guarantee comes from the database, not from
  an application check (`SELECT` then `INSERT` is a TOCTOU race).
* `request_hash` is the SHA-256 of the canonical body. The same key with a
  different payload is a client mistake → `409`, never silently processed.
* Both successful responses **and** deterministic 4xx responses are stored, so a
  retry returns an identical result instead of re-executing.
* Because the INSERT and the business logic live in the **same database
  transaction**, the idempotency row lock automatically forms the critical
  section: the second request waits until the first commits and then sees status
  `completed`. No extra distributed lock (Redis) is needed.
* `expires_at` (24h by default) bounds table growth; a janitor deletes expired keys.
* Replayed responses carry `Idempotency-Replayed: true` so clients know this was a
  replay, not a new execution.

---

## 6. Pagination (cursor-based)

`GET /v1/wallets/{id}/transactions?cursor=&limit=`

* Ordered `id DESC` (newest first), backed by the `(wallet_id, id DESC)` index.
* The cursor is `base64url("e:" + last_entry_id)`; opaque so the internal format
  can change without breaking clients.
* Next page: `WHERE wallet_id = $1 AND id < $cursor ORDER BY id DESC LIMIT $n+1`
  (`n+1` is used to decide `has_more` without `COUNT(*)`).
* **Why not `OFFSET`**: `OFFSET` gets slower for deep pages (it scans and then
  discards N rows) and can duplicate or skip rows when new rows are inserted
  between two requests — for an append-only ledger that is a correctness problem,
  not just a performance one.

---

## 7. HTTP Contract

### Success

* Single resource → the JSON object directly (no double envelope).
* Collection → `{"data": [...], "pagination": {"next_cursor": "...", "has_more": true}}`.

### Errors → RFC 7807 (Problem Details for HTTP APIs)

```json
{
  "type": "https://api.example.com/problems/insufficient-funds",
  "title": "Insufficient funds",
  "status": 409,
  "detail": "wallet 3f1c... has balance 5.000, required 10.000",
  "instance": "/v1/wallets/3f1c.../withdraw",
  "code": "insufficient_funds",
  "request_id": "01J...",
  "errors": [{"field": "amount_minor", "message": "must be greater than 0"}]
}
```

RFC 7807 is used because the format is standard (clients, SDKs, and gateways
already understand it), `type` + `code` give machines a stable handle, while
`title`/`detail` stay human-readable for debugging. The `errors[]` member is an
extension carrying per-field validation details. Every error is mapped in one
place: `pkg/apierror.From(err)`.

### Status catalogue

| HTTP | Code | When |
|---|---|---|
| 201 | — | Wallet/transaction created |
| 200 | — | Read, top-up/withdraw/transfer response, idempotent replay |
| 400 | `bad_request` | Invalid JSON, invalid UUID, missing header |
| 401 | `unauthorized` | Wrong/missing API key (when auth is enabled) |
| 404 | `not_found` | Wallet does not exist |
| 409 | `insufficient_funds` | Not enough balance |
| 409 | `wallet_not_active` | Wallet frozen/closed |
| 409 | `idempotency_conflict` | Same key, different payload |
| 409 | `idempotency_in_progress` | The twin request is still running |
| 422 | `currency_mismatch` | Cross-currency transfer |
| 422 | `self_transfer` | Transfer to the same wallet |
| 422 | `validation_error` | Amount ≤ 0, limit out of range, etc. |
| 429 | `rate_limited` | Token bucket exhausted |
| 500 | `internal_error` | Bug/unexpected error |
| 503 | `database_unavailable` | Readiness failed / DB down |

Note: "insufficient balance" maps to **409 Conflict** (the resource state
contradicts the request), matching the project specification. In strict REST terms
422 is also defensible; what matters is being consistent and documented.

---

## 8. Observability

* **Logging**: `log/slog` JSON handler, one log line per request
  (`request_id`, `method`, `route`, `status`, `duration_ms`, `bytes`, `error`).
  Values such as `request_id` and the logger are injected through
  `context.Context` so usecases and repositories are logged without extra parameters.
* **Metrics** (`/metrics`, Prometheus format):
  `http_requests_total{method,route,status}`, `http_request_duration_seconds`
  (histogram), `wallet_transactions_total{type,result}`, `db_pool_*`.
  The route label uses `http.Request.Pattern` (Go 1.23+), which bounds
  cardinality instead of exploding on path parameters.
* **Health**: `/healthz` (liveness, never touches the DB) and `/readyz`
  (readiness, `Ping` with a short statement timeout).

---

## 9. Configuration

A config struct built from environment variables (12-factor), parsed manually in
`internal/platform/config` and validated at startup (fail fast with a clear message).

| Env | Default | Purpose |
|---|---|---|
| `APP_ENV` | `development` | Determines log format (text vs JSON) and level |
| `HTTP_ADDR` | `:8080` | Listen address |
| `DATABASE_URL` | — | PostgreSQL DSN (required) |
| `DB_MAX_CONNS` / `DB_MIN_CONNS` | 10 / 2 | pgxpool size |
| `DB_CONN_MAX_LIFETIME` | 30m | Connection recycling |
| `REQUEST_TIMEOUT` | 10s | Per-request timeout |
| `SHUTDOWN_TIMEOUT` | 15s | Graceful shutdown budget |
| `MIGRATE_ON_START` | false | Run migrations during startup |
| `API_KEY` | empty | When set, every v1 endpoint requires the `X-API-Key` header |
| `RATE_LIMIT_RPS` / `RATE_LIMIT_BURST` | 50 / 100 | Per-IP token bucket |
| `IDEMPOTENCY_TTL` | 24h | Retention of idempotency keys |
| `SYSTEM_WALLET_ID` | `0000...0001` | World account |

---

## 10. Tech Stack Justification

| Need | Choice | Rationale and trade-off |
|---|---|---|---|
| HTTP server + routing | `net/http` + `http.ServeMux` | Since Go 1.22 `ServeMux` supports methods and wildcards (`POST /v1/wallets/{id}/topup`, `r.PathValue("id")`), so every route this service needs is covered without a dependency. Trade-off: no built-in route groups or middleware chaining and no regex routes — we write a ~10 line `handler.Chain()` helper; in exchange we get zero dependencies, `http.Request.Pattern` for metrics labels, and full compatibility with the standard library ecosystem (`httptest`, `http.Server.Shutdown`). Frameworks such as Gin/Fiber bring a large abstraction and dependency footprint; Fiber even replaces `net/http`, which breaks compatibility with standard middleware. |
| Structured logging | `log/slog` | The only logger in the standard library that integrates with `context.Context`, with a ready-made JSON handler and zero dependencies. Trade-off: slightly slower than Zap (which trades API ergonomics for speed) and a smaller hook ecosystem than logrus/Zerolog; for this service's throughput, logging is not the bottleneck. |
| Database driver | `pgx/v5` + `pgxpool` | Native PostgreSQL protocol (not through `database/sql`), first-class support for `JSONB`/`numeric`/arrays, a prepared statement cache, and batching — important because most request time is spent in the database. Trade-off: `pgx` is a PostgreSQL-only driver, so we lock ourselves to one vendor (we already rely on `SELECT FOR UPDATE`, `JSONB`, and constraint triggers, which are PostgreSQL-specific) and its API is more detailed than the generic but less efficient `database/sql`+`lib/pq`, which is no longer actively developed. |
| ORM | None (explicit SQL) | The queries stay visible: `FOR UPDATE`, lock ordering, debit/credit aggregation, and `ON CONFLICT` are the core of this system's correctness — hiding them behind an ORM would be dangerous. Explicit queries also avoid hidden N+1s and unintended preloading. Trade-off: more verbose (manual row→struct mapping, schema changes touch two places) and no ORM auto-migration — replaced by explicit SQL migrations (next row). |
| Migrations | `golang-migrate` (embedded `iofs`) | `.up.sql`/`.down.sql` files are version-controlled, reviewed like code, run in order, and have a rollback path; the binary embeds the migration folder so deployment stays a single artifact. Trade-off: a fairly large dependency tree because it supports many databases (we only use the `pgx/v5` driver), and migrations must be written by hand. ORM auto-migration is deliberately avoided because its DDL is implicit and risky on production databases (e.g. blocking `ALTER` statements). |
| Locking | Row-level lock (`SELECT ... FOR UPDATE`) inside a DB transaction | The lock lives in a resource shared by all replicas, so it stays correct when the service scales horizontally, and it is released automatically on commit/rollback/crash. Trade-off: transactions become longer (the lock is held while the transaction runs), so they must stay short (no network calls inside a transaction), and a `FOR UPDATE` over an unindexed `WHERE` can lock a table — therefore every access goes through the primary key. |
| Idempotency | `idempotency_keys` table + `UNIQUE(key)` | The "exactly once" guarantee is enforced by a database constraint rather than an application `SELECT`-then-`INSERT` (which has a TOCTOU race), and it stays correct across replicas with no Redis or distributed lock. Trade-off: one extra INSERT plus one UPDATE per mutating request, and the table needs periodic cleanup (`expires_at`). |
| Context propagation | `context.Context` in every layer | Carries deadlines/cancellation from the HTTP handler down to the database query (`pgx` cancels queries whose context is done) as well as request-scoped values (request id, logger). Trade-off: function signatures get longer and `ctx` must be threaded manually; nothing is automatic. |
| Graceful shutdown | `signal.NotifyContext` + `server.Shutdown` | In-flight requests get time to finish and database connections close cleanly → rollouts without failed requests. Trade-off: `SHUTDOWN_TIMEOUT` must be shorter than the orchestrator grace period, and shutdown can appear to hang when a component forgets to honour `ctx`. |
| Dependency injection | Manual constructor injection in `main.go` | One screen shows which object depends on what; there is no reflection or generated magic, and the compiler catches missing dependencies immediately. Trade-off: for a much larger service, manual wiring becomes repetitive and codegen (Wire/Fx) is more convenient; at this size transparency matters more. |
| Validation | Manual, in the usecase layer (plus a strict JSON decoder) | Business rules (sufficient funds, active wallet, same currency) belong to the usecase and must hold even when called from a CLI or a worker, not only from HTTP. Trade-off: verbose and prone to duplication compared to `go-playground/validator` tags; reduced by small `domain.Validate*` helpers that are unit tested. |
| Testing | `testing` + `net/http/httptest` + hand-written fakes | Hand-written fakes (structs implementing the usecase ports) are enough to test business logic without a database and without a heavy mocking framework; `httptest` exercises handlers end-to-end without opening a port. Trade-off: fakes must be maintained by hand and can drift from real behaviour → covered by integration tests that hit a real PostgreSQL. |
| Observability | `prometheus/client_golang` | Prometheus exposition format plus ready-made histograms that feed standard scraping and alerting; writing it by hand (`expvar`) would lose histograms and ready-made alerting rules. Trade-off: a fairly large dependency for a service this small, but latency percentiles are not realistic to build from scratch. |
| Containerization | Docker multi-stage → distroless final image | The builder stage holds the full toolchain; the final image only ships a static binary (built with `CGO_ENABLED=0`) with no shell or package manager → minimal attack surface and size, and the same binary as a local build. Trade-off: you cannot `exec` into the container to debug (use logs/metrics or a separate debug image). |
| Config | Struct from env vars, parsed manually | 12-factor; no config file to mount and no large Viper dependency just to read env vars. Trade-off: no YAML/remote config support, and non-string parsing (`int`, `duration`) must be written by hand — mitigated by small helpers that fail fast at startup on invalid values. |
| Rate limiting | `golang.org/x/time/rate` (token bucket) | A token bucket allows a short burst (a mobile client firing several requests at once) while enforcing the long-term average, and `rate.Limiter` keeps per-key state without a global lock. Trade-off: a fixed window is easier to understand and can be a simple counter, but it has the boundary burst problem (up to 2× the limit at the window edge); an in-memory limiter is also per-process, so it is not accurate across replicas (a global limit needs Redis). |
| Auth | Static API key (`X-API-Key`) as middleware | Enough to demonstrate request-scoped authentication middleware without dragging in OAuth/JWT, which is not the focus of this case study. Trade-off: there is no real per-user identity, so per-wallet authorization cannot be exercised yet — recorded as a limitation in §11. |

---

## 11. Trade-offs deliberately not taken

| Not taken | Why the current approach is enough |
|---|---|
| Message queue / outbox pattern (Kafka, RabbitMQ) | All balance and ledger reporting can be served **synchronously** with low latency; there is no second consumer (fraud detection, analytics) demanding an event stream yet. The outbox pattern adds a table, a publisher, and an idempotent consumer — complexity not paid for by today's requirements. If it becomes necessary, the cheapest hook is writing an event to an outbox table inside the same transaction. |
| Full event sourcing | An append-only ledger plus `balance_after` already provides a sufficient audit trail; event sourcing demands event versioning, replay, and snapshotting. |
| CQRS / a separate read replica | Read volume is still served by the primary; a replica adds replication complexity plus read-your-writes issues (you just transferred but the balance has not changed yet). |
| Distributed transactions (2PC) across services | No other service holds a balance; every mutation lives in a single database. |
| Multi-currency within one transfer | It requires an FX rate table, rate and spread decisions, which side absorbs rounding, and a P&L account. Cross-currency transfers are explicitly rejected with `422` instead of silently converting at a wrong rate. |
| Per-wallet authorization (ownership) | There is no identity provider yet; a static API key only proves "trusted client". The endpoint can be added by filling `owner_id` from the principal (not the body); `reqctx.Principal` is already in place. |
| Wallet soft delete / transaction reversal | Adds a complex state machine (posted → reversed) and compensating entries; a reversal endpoint is not needed for the E2E scenario. |
| Distributed rate limiting (Redis) | A per-process limit is enough for a single demo deployment; the consequence is that the effective limit is `RPS × replicas`. |

---

## 12. Next Stages

* **(b)** `internal/domain` + `internal/usecase` + port interfaces + business-logic unit tests.
* **(c)** `internal/http/{handler,middleware}` + `platform` (config, pgxpool, slog, migrate, metrics) + `cmd/api/main.go` wiring.
* **(d)** PostgreSQL integration tests, `docker-compose.yml`, `Dockerfile`, `requests.http`, `scripts/e2e.sh`, `README.md`.
* **(e)** `DECISIONS.md` (ADRs) + verification of `go build`, `go vet`, `go test ./...`, and the E2E flow.
