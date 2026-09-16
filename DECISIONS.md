# Architecture Decision Records — Wallet & Ledger Service

Compact ADR log for every decision that shapes this service. Each record states the
context, the decision, the consequences we accept, and the alternatives we rejected.

`docs/ARCHITECTURE.md` is the long-form companion document (ERD, schemas, diagrams,
full tech-stack rationale).

| ADR | Decision | Status |
|---|---|---|
| [001](#adr-001-pure-nethttp--servemux-instead-of-a-web-framework) | Pure `net/http` + `ServeMux` instead of a web framework | Accepted |
| [002](#adr-002-money-as-signed-bigint-minor-units) | Money as signed `BIGINT` minor units | Accepted |
| [003](#adr-003-double-entry-ledger-with-a-system-world-account) | Double-entry ledger with a system (world) account | Accepted |
| [004](#adr-004-pessimistic-row-level-locking-in-read-committed) | Pessimistic row-level locking in `READ COMMITTED` | Accepted |
| [005](#adr-005-canonical-lock-ordering-for-transfers) | Canonical lock ordering for transfers | Accepted |
| [006](#adr-006-idempotency-through-an-idempotency_keys-table) | Idempotency through an `idempotency_keys` table | Accepted |
| [007](#adr-007-clean-architecture-layering-with-explicit-wiring) | Clean Architecture layering with explicit wiring | Accepted |
| [008](#adr-008-pgx-driver-without-an-orm) | `pgx` driver without an ORM | Accepted |
| [009](#adr-009-explicit-embedded-sql-migrations) | Explicit, embedded SQL migrations | Accepted |
| [010](#adr-010-keyset-cursor-pagination) | Keyset (cursor) pagination | Accepted |
| [011](#adr-011-rfc-7807-for-every-error) | RFC 7807 for every error | Accepted |
| [012](#adr-012-structured-logging-with-logslog-and-prometheus-metrics) | Structured logging with `log/slog` + Prometheus metrics | Accepted |
| [013](#adr-013-distroless-container-image) | Distroless container image | Accepted |
| [014](#adr-014-synchronous-processing-no-message-queue-or-event-sourcing) | Synchronous processing: no message queue or event sourcing | Accepted |
| [015](#adr-015-trade-offs-deliberately-not-taken) | Trade-offs deliberately not taken | Accepted |

---

## ADR-001: Pure `net/http` + `ServeMux` instead of a web framework

**Status:** Accepted · **Context:** the service needs ~9 REST endpoints with path
parameters, method matching, and middleware.

**Decision.** Route with `http.ServeMux` using Go 1.22+ patterns
(`POST /v1/wallets/{walletID}/topup`, `r.PathValue("walletID")`) and compose
middleware with a ~10-line `Chain(func(http.Handler) http.Handler, ...)` helper.

**Why.** The routing features this service needs already exist in the standard
library, so a framework would add a dependency without adding capability.
Standard handlers keep every `net/http` tool working out of the box
(`httptest`, `http.Server.Shutdown`, `http.MaxBytesReader`,
`http.ResponseController`, and metrics/tracing middleware from any vendor).
We also get `http.Request.Pattern`, which is used as a bounded-cardinality
metrics/log label.

**Consequences.**
* Positive: zero routing dependencies; the entire HTTP surface is auditable in one
  file (`internal/http/handler/router.go`); no framework upgrade treadmill.
* Negative: route groups, regex routes, and per-group middleware do not exist — we
  register the chain per route instead; 405 responses are plain text by default, so
  a path-only fallback pattern is registered to keep the RFC 7807 contract.

**Alternatives rejected.** Gin/Echo/Chi/Gorilla Mux (extra dependency, no
capability gain); Fiber (replaces `net/http`, breaking standard middleware).
**Revisit if** routing logic grows route groups, regex constraints, or per-route
version negotiation.

---

## ADR-002: Money as signed `BIGINT` minor units

**Status:** Accepted · **Context:** balances, amounts, and the ledger must be exact
across top-up, transfer, withdraw, and reconciliation.

**Decision.** Store and transport every amount as an integer in minor units
(`Amount int64`), exposed as `amount_minor` in JSON. No `float64` and no decimal
string formatting anywhere in the request path.

**Why.** Binary floating point cannot represent decimal money exactly
(`0.1 + 0.2 != 0.3`) and the error accumulates in a balance. Keeping a single
integer representation from the JSON body to the `BIGINT` column also removes every
conversion boundary where precision could be lost or rounding decided implicitly.
`domain.Amount` is a named type, so amounts cannot be confused with counts or ids.

**Consequences.**
* Positive: exact arithmetic, trivial comparisons, and the database `CHECK` on
  balances is meaningful.
* Negative: clients must know the minor-unit scale of their currency (documented:
  scale 2, so `100000` with `IDR` means Rp 1.000,00); formatting is a presentation
  concern pushed to the client.

**Alternatives rejected.** `float64`/`NUMERIC` in Go structs (rounding risk or
driver-specific handling), decimal libraries (`shopspring/decimal`) that still end
up as strings over JSON with more moving parts.
**Revisit if** the product needs arbitrary-precision or sub-minor-unit accounting.

---

## ADR-003: Double-entry ledger with a system (world) account

**Status:** Accepted · **Context:** the ledger is the audit trail and the source of
truth for balances.

**Decision.** Every money movement writes exactly one `transactions` row plus two
`ledger_entries` rows (debit + credit). Money entering or leaving the system is
booked against a seeded system wallet (the "world account",
`SYSTEM_WALLET_ID`), whose balance is allowed to go negative by the
`wallets_balance_guard` check constraint. A `DEFERRABLE INITIALLY DEFERRED`
constraint trigger rejects any transaction whose `SUM(debit) <> SUM(credit)` at
COMMIT.

**Why.** Without a counterparty account, top-up/withdraw would be single-sided and
the ledger could not prove that money was conserved. The deferred trigger turns
"every journal must balance" into a database invariant, not an application
convention: a bug in a future code path cannot corrupt the ledger.

**Consequences.**
* Positive: `SELECT sum(balance_minor) FROM wallets` must always be `0` (verified in
  the E2E script and in every integration test); the `transaction_totals` view
  provides an instant reconciliation query; every balance is reconstructible from
  immutable entries.
* Negative: the world account balance becomes large and negative and must be
  understood during audits; single-sided adjustments are impossible by design; the
  ledger grows forever and will eventually need partitioning/archiving.

**Alternatives rejected.** Mutable-only balances without a ledger (no audit trail);
single-entry records with a signed amount (auditors cannot distinguish direction
from convention); a separate "external" table instead of a wallet row (two code
paths for the same concept).
**Revisit if** a full reversal/void workflow is added — it will be a compensating
entry pair, never an `UPDATE`.

---

## ADR-004: Pessimistic row-level locking in `READ COMMITTED`

**Status:** Accepted · **Context:** concurrent requests may target the same wallet
simultaneously; the service runs as multiple replicas.

**Decision.** Every mutating operation runs inside one database transaction that
first claims the idempotency key, then locks the involved wallet rows with
`SELECT ... FOR UPDATE` (through the primary key), validates, and only then
mutates balances and writes the ledger. Isolation stays at `READ COMMITTED`
(set explicitly by `postgres.UnitOfWork`).

**Why.** A process-local `sync.Mutex` cannot protect a row that two replicas may
serve at the same time. The lock must live in the shared resource — the PostgreSQL
row — where it is also released automatically on commit, rollback, or crash.
`READ COMMITTED` + an explicit row lock gives the guarantee we need; `SERIALIZABLE`
would add random `40001` failures that the application would have to retry, and
optimistic locking (`WHERE version = $n`) would force retry loops precisely on the
hottest rows (a popular wallet), trading predictable waiting for unpredictable
retries.

**Consequences.**
* Positive: no lost updates and no negative balances, proven by the integration
  tests; `statement_timeout` and a request timeout bound how long a lock is held; a
  warning is logged when a transaction exceeds 200 ms.
* Negative: transactions hold locks, so they must stay short — no network calls
  inside a transaction; a `FOR UPDATE` without an indexed predicate could lock far
  more than intended, which is why every lock goes through the primary key.

**Alternatives rejected.** `sync.Mutex` (broken across replicas); `SERIALIZABLE`
(retry loops); optimistic locking (retry storms on hot rows); a single-writer queue
(throughput ceiling and a new failure mode).
**Revisit if** write contention per wallet becomes a measured bottleneck — the next
step would be a per-wallet serialization mechanism, not a weaker isolation level.

---

## ADR-005: Canonical lock ordering for transfers

**Status:** Accepted · **Context:** a transfer touches two wallets, and opposite
transfers (`A→B` while `B→A`) can deadlock.

**Decision.** `lockWallets` de-duplicates the involved wallets and sorts them by id
before issuing one `SELECT ... FOR UPDATE` per wallet, so every transaction
acquires locks in the same global order.

**Why.** The ABBA deadlock is caused by inconsistent lock ordering; sorting ids is
the cheapest possible fix and costs nothing at runtime. PostgreSQL's
`deadlock_timeout` remains as a backstop but is not part of the correctness
argument.

**Consequences.**
* Positive: `TestIntegrationConcurrentBidirectionalTransfers` runs 15 transfers in
  each direction concurrently and must complete without a single `40P01`, while
  balances end exactly where they started.
* Negative: lock acquisition is one extra round-trip per wallet instead of a single
  statement; the invariant is easy to break accidentally, so it is documented in
  the port comment (`WalletRepository.GetByIDForUpdate`) and covered by a test.

**Alternatives rejected.** Relying on deadlock detection and retrying (turns a
correctness property into an error-handling path); locking a coarser scope such as
all wallets of an owner (unnecessary contention).
**Revisit if** an operation must lock more than a handful of wallets (e.g. batch
payouts) — then a deterministic global ordering helper becomes mandatory API.

---

## ADR-006: Idempotency through an `idempotency_keys` table

**Status:** Accepted · **Context:** clients retry on timeouts or double submits;
a retried top-up must never execute twice.

**Decision.** Mutating endpoints require an `Idempotency-Key` header. Inside the
same transaction as the business effect, the server inserts a row into
`idempotency_keys` (`key TEXT PRIMARY KEY`, `endpoint`, `request_hash`, `status`,
`expires_at`) using `INSERT ... ON CONFLICT DO NOTHING`. A successful claim means
"execute"; a failed claim means "read the stored record": same endpoint + same
request hash and status `completed` → replay the stored response with
`Idempotency-Replayed: true`; a different hash → `409 idempotency_conflict`; still
`in_progress` → `409 idempotency_in_progress`.

**Why.** The guarantee comes from a database constraint, not from an application
`SELECT`-then-`INSERT` (which races). Because the claim and the business effect
share one transaction, the row lock *is* the critical section: a concurrent twin
request blocks on the unique index until the first commits, then sees the stored
result — no Redis, no distributed lock, and no window where a key is "claimed" but
the effect is not committed. `ON CONFLICT DO NOTHING` (rather than catching a
unique violation) matters because any error aborts a PostgreSQL transaction.

**Consequences.**
* Positive: 10 concurrent identical requests produce exactly 1 execution and 9
  replays (proven in the integration test); the stored response is the serialized
  representation of the same DTO, so replays are identical by construction.
* Negative: one extra INSERT + UPDATE per mutating request; the table must be
  cleaned (a janitor deletes rows past `IDEMPOTENCY_TTL`, 24h); reusing a key for a
  different payload is a client error that surfaces as 409. A failed business
  operation releases the key, so retrying it after fixing the cause executes
  normally — intentional, and covered by a test.

**Alternatives rejected.** In-memory dedupe (per replica, lost on restart); Redis
`SETNX` (another infrastructure dependency, and no atomic storage of the response
next to the effect); relying on client discipline (not a guarantee).
**Revisit if** idempotency must span multiple services, where a shared store or an
outbox would be needed.

---

## ADR-007: Clean Architecture layering with explicit wiring

**Status:** Accepted · **Context:** business rules (balances, double-entry,
idempotency) must stay testable and independent of infrastructure.

**Decision.** Four layers — `domain` (no external imports), `usecase` (declares the
ports it needs), adapters (`repository/postgres`, `http/handler`), and `platform`
(config, logger, pool, migrate, metrics). Dependencies point inwards only;
`cmd/api/main.go` wires everything with constructor injection.

**Why.** The correctness-critical logic must be provable without a database: the
usecase only sees interfaces, so its tests run against in-memory fakes that
deliberately mimic database behaviour (copies on read, snapshot/rollback,
serialization). Adapters stay replaceable: the HTTP layer never imports pgx and the
repository never imports `net/http`. Explicit wiring in one file means the
dependency graph is readable without tooling, and the compiler catches missing
dependencies — no reflection, no codegen, no runtime surprises.

**Consequences.**
* Positive: domain 66% / usecase 86% coverage from pure unit tests; a fake or a
  second adapter (e.g. an in-memory repository) needs no production changes;
  onboarding reads one file to understand how the service is assembled.
* Negative: more packages and interfaces than a flat layout; every new capability
  touches port + adapter + handler. The interface count must be kept small or the
  indirection stops paying for itself.

**Alternatives rejected.** Flat `handler → service → repository` without
interfaces (untestable without a database, framework knowledge leaks); a DI
container or codegen (hidden wiring); hexagonal overkill with one package per port.
**Revisit if** the service grows into multiple bounded contexts — then each context
gets the same shape rather than a different one.

---

## ADR-008: `pgx` driver without an ORM

**Status:** Accepted · **Context:** the queries themselves encode the correctness
properties (`FOR UPDATE`, lock ordering, `ON CONFLICT`, debit/credit aggregation).

**Decision.** Use `pgx/v5` with `pgxpool` and hand-written SQL in
`internal/repository/postgres`. Scan into primitives and convert to domain types
explicitly. Map driver errors to domain sentinels in one place (`mapError`).

**Why.** SQL that is visible in review is exactly what this service is judged on —
`SELECT ... FOR UPDATE`, the `LATERAL` counterparty join, and
`ON CONFLICT DO NOTHING` must not be hidden behind a query builder. `pgx` is the
native protocol implementation (prepared statement cache, batching, first-class
`JSONB`), which matters because most request latency is database latency. Explicit
scanning avoids reflection surprises for named types and makes a schema change fail
loudly rather than silently.

**Consequences.**
* Positive: predictable queries, no hidden N+1s, no lazy-loading surprises; error
  translation is testable and centralized; repository coverage of 70% comes from
  real SQL integration tests.
* Negative: verbose (manual row→struct mapping, two places to update on schema
  changes); PostgreSQL-specific by design (`FOR UPDATE`, `JSONB`, constraint
  triggers); no migration generation.

**Alternatives rejected.** GORM/sqlc/ent (generated or hidden SQL for the queries
that matter most); `database/sql` + `lib/pq` (generic but slower and no longer
actively developed).
**Revisit if** the read model grows many projections — generated SQL code could
then reduce boilerplate without hiding the critical queries.

---

## ADR-009: Explicit, embedded SQL migrations

**Status:** Accepted · **Context:** the schema (tables, check constraints, the
deferred balance trigger, the seed row) is part of the correctness argument.

**Decision.** Version-controlled `migrations/NNNNNN_name.{up,down}.sql` files
executed by `golang-migrate` against an `iofs` source that embeds the folder into
the binary. Migrations run via `MIGRATE_ON_START=true` or
`api migrate up|down|version`.

**Why.** DDL reviewed like code, with a rollback path and a version recorded in
`schema_migrations`. Embedding keeps deployment a single artifact (no migration
folder to mount) and guarantees the binary and the schema move together. ORM
auto-migration was rejected because its DDL is implicit and can block a production
table without review.

**Consequences.**
* Positive: `docker compose up` produces a ready schema; a dirty state is detected
  and reported; the trigger/view/seed that enforce invariants are reviewable.
* Negative: migrations are hand-written; the `golang-migrate` dependency tree is
  large (it supports many databases) and pulls test-only modules into `go.sum`;
  `down` migrations that would destroy ledger data cannot be automatic.

**Alternatives rejected.** ORM auto-migrate (implicit, risky); applying raw files
by hand or via `docker-entrypoint-initdb.d` only (would not survive schema
evolution); a custom migrator (reinventing locking and version tracking).
**Revisit if** zero-downtime requirements grow — expand/contract steps and
`NOT VALID` constraints would be added to the migration files.

---

## ADR-010: Keyset (cursor) pagination

**Status:** Accepted · **Context:** transaction history is an append-only ledger
that clients page through while new rows are being written.

**Decision.** Page over `ledger_entries` by `id DESC` with
`WHERE wallet_id = $1 AND (id < $cursor) LIMIT $n+1`. The cursor is an opaque
`base64url("e:" + id)`. `limit+1` decides `has_more` without `COUNT(*)`.

**Why.** With `OFFSET`, rows inserted between two requests shift the window, which
can duplicate or skip entries in a financial history — a correctness problem, not
just a performance one. Keyset pagination also has constant cost: the
`(wallet_id, id DESC)` index jumps straight to the position instead of scanning and
discarding N rows. The cursor is opaque so the strategy can change (e.g. to a
`(created_at, id)` keyset) without breaking clients.

**Consequences.**
* Positive: no gaps or duplicates under concurrent writes (covered by an
  integration test over 7 entries in pages of 3); stable latency for deep pages.
* Negative: clients cannot jump to page N and there is no total count; the cursor
  leaks the entry id volume (mitigated by encoding); an invalid cursor is a `400`.

**Alternatives rejected.** `OFFSET`/`LIMIT` (drift + slower deep pages); keyset on
`created_at` alone (not unique → ties can still skip rows).
**Revisit if** clients demand random access or totals — that would be a separate
reporting endpoint, not a change to the ledger history API.

---

## ADR-011: RFC 7807 for every error

**Status:** Accepted · **Context:** clients need a predictable error contract across
validation, business-rule, and infrastructure failures.

**Decision.** Every error response is RFC 7807 Problem Details with the extensions
`code` (stable machine handle), `request_id` (correlation), and `errors[]`
(per-field validation). A single mapper (`pkg/apierror.From`) translates domain
sentinels and transport errors into that shape using `errors.Is/As`.

**Why.** One mapping point means every endpoint is consistent by construction and
the domain stays free of HTTP status codes. `errors.Is/As` with wrapped errors
(`%w`) lets a failure be classified at any layer without string matching or
parallel error taxonomies.

**Consequences.**
* Positive: `type`/`code` are stable for clients, `title`/`detail` stay readable;
  5xx details are never leaked while the full cause is logged; the whole mapping
  table is unit tested (21 cases).
* Negative: richer response bodies than a bare `{"error": "..."}`; 4xx/5xx
  classification must be maintained as new errors appear — enforced by a table
  test.

**Alternatives rejected.** Plain `{"error": ...}` (no machine handle, no
correlation); gRPC-style status codes over HTTP (non-standard for REST clients);
per-handler error formatting (guaranteed drift).
**Revisit if** an API gateway needs a different error envelope — it would be a
translation layer at the edge, not a change to the domain.

---

## ADR-012: Structured logging with `log/slog` and Prometheus metrics

**Status:** Accepted · **Context:** one structured line per request and operational
metrics are required for a service that moves money.

**Decision.** `log/slog` with a JSON handler in production / text in development,
one log line per request (`request_id`, `method`, `route`, `status`, `duration`,
`bytes`, `client_ip`), and a private Prometheus registry exposing
`http_requests_total{method,route,status}`,
`http_request_duration_seconds`, `http_requests_in_flight`,
`wallet_transactions_total{type,result}`, and pool gauges.

**Why.** `slog` is the only context-aware structured logger in the standard library
— zero dependencies, JSON out of the box — and the logger is propagated through
`context.Context`, so any layer can log with the request context already attached.
The route label uses `http.Request.Pattern`, so cardinality is bounded by the
number of routes rather than by traffic; `wallet_transactions_total{result}`
distinguishes `success` from `replayed`, which makes retry behaviour observable
without new code.

**Consequences.**
* Positive: latency percentiles and error rates are scrape-ready; logs correlate
  with responses through `request_id`; a private registry keeps metrics isolated
  (tests get their own).
* Negative: `client_golang` is a sizeable dependency for a service this small;
  `slog` is marginally slower than Zap; pool gauges must stop reading statistics
  before the pool closes (`MarkShuttingDown`), which is a shutdown-ordering
  requirement.

**Alternatives rejected.** Zap/Zerolog (extra dependency for no functional gain);
`expvar` (no histograms, no ready-made alerting); ad-hoc `log.Printf` (unparseable).
**Revisit if** tracing is introduced — OpenTelemetry would then sit alongside this,
reusing the same request id.

---

## ADR-013: Distroless container image

**Status:** Accepted · **Context:** the deliverable must run with one command and
ship the smallest reasonable attack surface.

**Decision.** Multi-stage build: `golang:1.26-alpine` compiles a static binary
(`CGO_ENABLED=0`, `-trimpath`, version injected via ldflags); the runtime stage is
`gcr.io/distroless/static-debian12:nonroot`. The binary exposes a `healthcheck`
subcommand used as `HEALTHCHECK` (the image has no shell or curl). `docker compose
up` starts PostgreSQL 16 with a readiness gate, then the service with
`MIGRATE_ON_START=true`.

**Why.** distroless keeps CA certificates, tzdata, and a non-root user (which
`scratch` lacks) while removing a shell and package manager. A self-health-checking
binary keeps orchestration working without adding tooling to the image. The image
is ~27 MB and starts in under a second.

**Consequences.**
* Positive: minimal attack surface; deterministic image (same binary as a local
  build); health/readiness integrate with `docker compose` and any orchestrator.
* Negative: no shell for debugging (use logs/metrics or a separate debug image);
  a vendor-specific base image; the image must be rebuilt to change configuration.

**Alternatives rejected.** `scratch` (no certs/tzdata/user); a full distro image
(needless surface); `curl`-based health checks (would require adding a shell).
**Revisit if** the platform mandates a signed/distroless variant or a different
registry.

---

## ADR-014: Synchronous processing (no message queue or event sourcing)

**Status:** Accepted · **Context:** every endpoint must answer with the final state
of the ledger it just changed.

**Decision.** All money movements complete synchronously inside one database
transaction and return the post-mutation balance. No message broker, no outbox, no
event bus, no asynchronous consumers.

**Why.** The read-your-writes guarantee is a product requirement: a client that has
just transferred must see the new balance immediately, so an asynchronous
projection would have to be waited on anyway. There is currently no second consumer
(fraud scoring, notifications, analytics) whose latency budget would justify a
broker. Within one database, a transaction already provides atomicity, ordering,
and durability — a queue would add a second source of truth and a new failure mode
(duplicate or lost events) without buying anything at this scale.

**Consequences.**
* Positive: no broker to operate; failures are visible in the response rather than
  in a dead-letter queue; the idempotency table already gives consumers an
  at-most-once effect if a queue is ever added.
* Negative: request latency includes all work (kept low by narrow transactions);
  slow or external side effects (notifications, external settlement) cannot be part
  of the request path later without introducing one.

**Alternatives rejected.** Kafka/RabbitMQ now (operational cost with no consumer
yet); event sourcing as the storage model (replay, versioning, and snapshotting for
a problem a ledger plus `balance_after` already solves); a transactional outbox now
(adds a table and a publisher for events nobody reads).
**Revisit if** a second consumer appears or external settlement must be triggered —
then the cheapest step is a transactional outbox written inside the same
transaction, keeping the API synchronous.

---

## ADR-015: Trade-offs deliberately not taken

**Status:** Accepted · **Context:** the case study rewards correct, defensible
scope over feature count.

| Not taken | Why it is out of scope now | Cheapest future hook |
|---|---|---|
| CQRS / read replica | Read volume is served by the primary; a replica introduces replication lag and read-your-writes problems | Route history queries to a replica once volume demands it |
| Multi-currency transfers | Requires FX rates, spread ownership, rounding policy, and a P&L account; a mismatch returns `422` instead of converting wrongly | An explicit FX quote + conversion entry pair |
| Per-wallet authorization | There is no identity provider; the static API key proves a trusted client, not ownership | Fill `owner_id` from `reqctx.Principal` and check it in the usecase |
| Distributed rate limiting | Effective limit is per replica; acceptable for a single deployment | Swap the in-memory limiter for a Redis-backed store |
| Reversal / void endpoint | Needs a state machine and compensating entries; not required by the E2E scenario | Post a compensating transaction pair referencing the original |
| Wallets as first-class accounts with KYC/limits | Out of scope for a ledger-focused service | Add a policy layer before the movement usecase |
| Retention/archival of old ledger rows | Not needed at demo volume | Partition `ledger_entries` by month and archive cold partitions |

---

## Verification accompanying this log

Every decision above is backed by an executable check, not only by prose:

| Decision | Evidence |
|---|---|
| ADR-003 (double-entry) | `transaction_totals` view + `sum(balance_minor) = 0` assertion in `scripts/e2e.sh` and integration tests; the trigger was verified manually against a hand-written unbalanced journal |
| ADR-004 (locking) | `TestIntegrationConcurrentBidirectionalTransfers`, `TestIntegrationInsufficientFundsLeavesNoTrace` |
| ADR-006 (idempotency) | `TestIntegrationConcurrentTopUpWithSameKey` (10 concurrent requests → 1 execution, 9 replays), plus unit tests for replay and conflict |
| ADR-010 (pagination) | `TestIntegrationHistoryCursorPaginationHasNoGapsOrDuplicates` |
| ADR-011 (error contract) | 21-case mapping table in `pkg/apierror`, plus handler tests asserting status codes and problem fields |
| ADR-001/012 (routing, metrics) | `router_test.go` (registration, 404/405 problems, `Allow` header) and the metrics assertion in `middleware_test.go` |
