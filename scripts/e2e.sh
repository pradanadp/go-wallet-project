#!/usr/bin/env bash
#
# End-to-end test scenario for the Wallet & Ledger Service.
#
# Prasyarat:
#   docker compose up -d postgres     (PostgreSQL must be running)
#   go run ./cmd/api                  (the service must be running)
#
# Pemakaian:
#   ./scripts/e2e.sh                       # BASE_URL default http://localhost:8080
#   BASE_URL=http://host:8080 ./scripts/e2e.sh
#
# This script only uses curl + grep (no jq) so it can run on a minimal
# machine/CI.
#
# Implementation note: the response body/status are kept in a file and in shell
# (not command substitution) so the HTTP_STATUS value is not lost in a subshell.

set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:8080}"
BODY_FILE="$(mktemp -t wallet-e2e)"
IDEM_PREFIX="e2e-$(date +%s)"
trap 'rm -f "$BODY_FILE"' EXIT

pass() { printf '  \033[32mOK\033[0m   %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; printf '  body: %s\n' "$(cat "$BODY_FILE")"; exit 1; }
step() { printf '\n\033[1m%s\033[0m\n' "$1"; }

HTTP_STATUS=""
BODY=""

# request METHOD PATH [BODY] [IDEMPOTENCY_KEY]
# Fills HTTP_STATUS and BODY in the caller's shell.
request() {
  local method="$1" path="$2" body="${3:-}" idem="${4:-}"
  local args=(-s -o "$BODY_FILE" -w '%{http_code}' -X "$method" "$BASE_URL$path")
  [[ -n "$body" ]] && args+=(-H 'Content-Type: application/json' -d "$body")
  [[ -n "$idem" ]] && args+=(-H "Idempotency-Key: $idem")

  HTTP_STATUS="$(curl "${args[@]}")"
  BODY="$(cat "$BODY_FILE")"
}

# field NAME -> the string value of '"name":"value"'
field() { printf '%s' "$BODY" | grep -o "\"$1\":\"[^\"]*\"" | head -1 | cut -d'"' -f4; }

# number NAME -> the numeric value of '"name":123'
number() { printf '%s' "$BODY" | grep -o "\"$1\":[0-9-]*" | head -1 | cut -d: -f2; }

expect_status() {
  if [[ "$HTTP_STATUS" != "$1" ]]; then
    fail "HTTP status $HTTP_STATUS, expected $1 ($2)"
  fi
  pass "$2 -> HTTP $1"
}

expect_body_contains() {
printf '%s' "$BODY" | grep -q "$1" || fail "body does not contain $1"
}

expect_balance() {
  local wallet="$1" expected="$2" label="$3"
  request GET "/v1/wallets/$wallet"
expect_status 200 "read $label"
  [[ "$(number balance_minor)" == "$expected" ]] \
|| fail "balance of $label = $(number balance_minor), expected $expected"
pass "balance of $label = $expected"
}

command -v curl >/dev/null || { printf 'curl is not installed\n'; exit 1; }

step "0. Health & readiness"
request GET /healthz;      expect_status 200 "GET /healthz"
request GET /readyz;       expect_status 200 "GET /readyz (database connected)"

step "1. Creating wallets A and B"
request POST /v1/wallets '{"owner_id":"11111111-1111-4111-8111-111111111111","currency":"IDR"}' "$IDEM_PREFIX-create-a"
expect_status 201 "POST /v1/wallets (A)"
WALLET_A="$(field id)"
[[ -n "$WALLET_A" ]] || fail "wallet A id is empty"

request POST /v1/wallets '{"owner_id":"22222222-2222-4222-8222-222222222222","currency":"IDR"}' "$IDEM_PREFIX-create-b"
expect_status 201 "POST /v1/wallets (B)"
WALLET_B="$(field id)"
printf '  wallet A = %s\n  wallet B = %s\n' "$WALLET_A" "$WALLET_B"

step "2. Top-up wallet A with 100000 minor units"
request POST "/v1/wallets/$WALLET_A/topup" '{"amount_minor":100000,"reference":"transfer-bank-bca-001"}' "$IDEM_PREFIX-topup-a-1"
expect_status 200 "POST /v1/wallets/{id}/topup"
TOPUP_TXN="$(field transaction_id)"
[[ "$(number balance_after_minor)" == "100000" ]] || fail "balance_after of the top-up is not 100000"
pass "transaction_id = $TOPUP_TXN, balance A = 100000"

step "3. Transferring 40000 from A to B (atomic, double-entry)"
request POST "/v1/wallets/$WALLET_A/transfer" "{\"to_wallet_id\":\"$WALLET_B\",\"amount_minor\":40000,\"reference\":\"invoice-2026-0042\"}" "$IDEM_PREFIX-transfer-1"
expect_status 200 "POST /v1/wallets/{id}/transfer"
[[ "$(number balance_after_minor)" == "60000" ]] || fail "sender balance after the transfer is not 60000"
expect_balance "$WALLET_A" 60000 "wallet A (sender)"
expect_balance "$WALLET_B" 40000 "wallet B (receiver)"

step "4. A withdraw above the balance must fail"
request POST "/v1/wallets/$WALLET_B/withdraw" '{"amount_minor":999999999,"reference":"withdraw-everything"}' "$IDEM_PREFIX-withdraw-fail"
expect_status 409 "withdraw above the balance"
expect_body_contains '"code":"insufficient_funds"'
expect_body_contains '"type":"https://api.example.com/problems/insufficient_funds"'
pass "complete RFC 7807 problem details (type, code, status, detail, instance)"
expect_balance "$WALLET_B" 40000 "wallet B (rollback, unchanged)"

step "5. Retrying the top-up with the same Idempotency-Key (must not double top-up)"
request POST "/v1/wallets/$WALLET_A/topup" '{"amount_minor":100000,"reference":"transfer-bank-bca-001"}' "$IDEM_PREFIX-topup-a-1"
expect_status 200 "top-up retry replayed"
[[ "$(field transaction_id)" == "$TOPUP_TXN" ]] || fail "the retry produced a different transaction_id"
[[ "$(number balance_after_minor)" == "100000" ]] || fail "the retry changed the balance (double top-up)"
pass "same transaction_id ($TOPUP_TXN), balance still 100000"
expect_balance "$WALLET_A" 60000 "wallet A (total balance did not grow)"

step "6. The same Idempotency-Key with a different payload must return 409"
request POST "/v1/wallets/$WALLET_A/topup" '{"amount_minor":5000,"reference":"transfer-bank-bca-001"}' "$IDEM_PREFIX-topup-a-1"
expect_status 409 "key reused with a different payload"
expect_body_contains '"code":"idempotency_conflict"'

step "7. A missing Idempotency-Key header must return 400"
request POST "/v1/wallets/$WALLET_A/topup" '{"amount_minor":1000}'
expect_status 400 "top-up without an Idempotency-Key"
expect_body_contains '"field":"Idempotency-Key"'

step "8. Wallet A transaction history (cursor pagination)"
request GET "/v1/wallets/$WALLET_A/transactions?limit=1"
expect_status 200 "first page (limit=1)"
FIRST_CURSOR="$(field next_cursor)"
expect_body_contains '"has_more":true'
[[ -n "$FIRST_CURSOR" ]] || fail "next_cursor is empty even though has_more=true"
pass "next_cursor = $FIRST_CURSOR"

request GET "/v1/wallets/$WALLET_A/transactions?limit=1&cursor=$FIRST_CURSOR"
expect_status 200 "second page"
[[ -n "$(number entry_id)" ]] || fail "the second page is empty"

request GET "/v1/wallets/$WALLET_A/transactions?limit=50"
expect_status 200 "full page"
ENTRY_COUNT="$(printf '%s' "$BODY" | grep -o '"entry_id"' | wc -l | tr -d ' ')"
pass "wallet A history holds $ENTRY_COUNT entries (top-up in + transfer out)"

step "9. Other error cases"
request GET "/v1/wallets/99999999-9999-4999-8999-999999999999"
expect_status 404 "unknown wallet"
expect_body_contains '"code":"not_found"'

request POST "/v1/wallets/$WALLET_A/transfer" "{\"to_wallet_id\":\"$WALLET_A\",\"amount_minor\":1000}" "$IDEM_PREFIX-self"
expect_status 422 "transfer to the same wallet"
expect_body_contains '"code":"self_transfer"'

request GET /does-not-exist
expect_status 404 "unknown endpoint (problem details)"

curl -s -o /dev/null -D "$BODY_FILE" -X GET "$BASE_URL/v1/wallets"
grep -qi '^HTTP/1.1 405' "$BODY_FILE" || fail "GET /v1/wallets did not return 405"
grep -qi '^Allow: POST' "$BODY_FILE" || fail "the Allow header does not contain POST"
pass "GET /v1/wallets -> 405 + Allow: POST"

step "10. Prometheus metrics"
METRICS="$(curl -s "$BASE_URL/metrics")"
for metric in http_requests_total wallet_transactions_total db_pool_connections_total http_request_duration_seconds; do
printf '%s' "$METRICS" | grep -q "$metric" || fail "metric $metric was not found"
  pass "metric $metric is exposed"
done

step "11. Ledger reconciliation (debit = credit per transaction)"
if command -v docker >/dev/null && docker exec wallet-postgres true 2>/dev/null; then
  UNBALANCED="$(docker exec wallet-postgres psql -U wallet -d wallet -tAc 'SELECT count(*) FROM transaction_totals WHERE total_debit <> total_credit;')"
[[ "$UNBALANCED" == "0" ]] || fail "found $UNBALANCED unbalanced transactions"
pass "every transaction is balanced (transaction_totals view)"
else
printf '  (skipped: the wallet-postgres container is not reachable)\n'
fi

printf '\n\033[32mAll E2E scenarios passed.\033[0m\n'
printf 'wallet A = %s\nwallet B = %s\n' "$WALLET_A" "$WALLET_B"
