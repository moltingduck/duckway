#!/bin/bash
set -euo pipefail

RUNTIME="${CONTAINER_RUNTIME:-podman}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
RUN_ID="$(date +%s)-$$"
PREFIX="duckway-migration-e2e-$RUN_ID"
NETWORK="$PREFIX-net"
SQLITE_VOLUME="$PREFIX-sqlite"
POSTGRES_VOLUME="$PREFIX-postgres"
SQLITE_CONTAINER="$PREFIX-sqlite"
POSTGRES_CONTAINER="$PREFIX-postgres"
ADMIN_CONTAINER="$PREFIX-admin"
GATEWAY_CONTAINER="$PREFIX-gateway"
MOCK_CONTAINER="$PREFIX-mock"
SERVER_IMAGE="$PREFIX-server"
ADMIN_IMAGE="$PREFIX-admin"
GATEWAY_IMAGE="$PREFIX-gateway"
MIGRATOR_IMAGE="$PREFIX-migrator"
PASSWORD="migration-e2e-password"
REQUEST_LOG_COUNT="${DUCKWAY_MIGRATION_E2E_REQUEST_LOGS:-175000}"
ORPHAN_LOG_COUNT="${DUCKWAY_MIGRATION_E2E_ORPHAN_LOGS:-6000}"
SKIP_REQUEST_LOGS="${DUCKWAY_MIGRATION_E2E_SKIP_REQUEST_LOGS:-false}"
EXPECTED_REQUEST_LOG_COUNT="$((REQUEST_LOG_COUNT + 1))"
COOKIE_FILE="$(mktemp)"
PG_COOKIE_FILE="$(mktemp)"
CREATED_NETWORK=false
CREATED_SQLITE_VOLUME=false
CREATED_POSTGRES_VOLUME=false
CREATED_SQLITE_CONTAINER=false
CREATED_POSTGRES_CONTAINER=false
CREATED_ADMIN_CONTAINER=false
CREATED_GATEWAY_CONTAINER=false
CREATED_MOCK_CONTAINER=false

cleanup() {
  if [ "$CREATED_ADMIN_CONTAINER" = true ]; then "$RUNTIME" stop "$ADMIN_CONTAINER" >/dev/null 2>&1 || true; "$RUNTIME" rm "$ADMIN_CONTAINER" >/dev/null 2>&1 || true; fi
  if [ "$CREATED_GATEWAY_CONTAINER" = true ]; then "$RUNTIME" stop "$GATEWAY_CONTAINER" >/dev/null 2>&1 || true; "$RUNTIME" rm "$GATEWAY_CONTAINER" >/dev/null 2>&1 || true; fi
  if [ "$CREATED_MOCK_CONTAINER" = true ]; then "$RUNTIME" stop "$MOCK_CONTAINER" >/dev/null 2>&1 || true; "$RUNTIME" rm "$MOCK_CONTAINER" >/dev/null 2>&1 || true; fi
  if [ "$CREATED_SQLITE_CONTAINER" = true ]; then "$RUNTIME" stop "$SQLITE_CONTAINER" >/dev/null 2>&1 || true; "$RUNTIME" rm "$SQLITE_CONTAINER" >/dev/null 2>&1 || true; fi
  if [ "$CREATED_POSTGRES_CONTAINER" = true ]; then "$RUNTIME" stop "$POSTGRES_CONTAINER" >/dev/null 2>&1 || true; "$RUNTIME" rm "$POSTGRES_CONTAINER" >/dev/null 2>&1 || true; fi
  if [ "$CREATED_SQLITE_VOLUME" = true ]; then "$RUNTIME" volume rm "$SQLITE_VOLUME" >/dev/null 2>&1 || true; fi
  if [ "$CREATED_POSTGRES_VOLUME" = true ]; then "$RUNTIME" volume rm "$POSTGRES_VOLUME" >/dev/null 2>&1 || true; fi
  if [ "$CREATED_NETWORK" = true ]; then "$RUNTIME" network rm "$NETWORK" >/dev/null 2>&1 || true; fi
  "$RUNTIME" rmi "$SERVER_IMAGE" "$ADMIN_IMAGE" "$GATEWAY_IMAGE" "$MIGRATOR_IMAGE" >/dev/null 2>&1 || true
  rm -f "$COOKIE_FILE" "$PG_COOKIE_FILE"
}
trap cleanup EXIT

for command in "$RUNTIME" curl jq; do
  command -v "$command" >/dev/null || { echo "missing required command: $command" >&2; exit 1; }
done

wait_for_url() {
  local url="$1"
  for _ in $(seq 1 60); do
    curl -fsS "$url" >/dev/null 2>&1 && return 0
    sleep 1
  done
  return 1
}

container_base_url() {
  local container="$1"
  local port
  port="$($RUNTIME port "$container" 8080/tcp | tail -1)"
  printf 'http://%s' "$port"
}

cd "$ROOT"
echo "[1/7] Building Duckway server and migrator images"
"$RUNTIME" build --target server -t "$SERVER_IMAGE" .
"$RUNTIME" build --target admin -t "$ADMIN_IMAGE" .
"$RUNTIME" build --target gateway -t "$GATEWAY_IMAGE" .
"$RUNTIME" build --target postgres-migrator -t "$MIGRATOR_IMAGE" .

echo "[2/7] Starting Duckway with SQLite"
"$RUNTIME" network create "$NETWORK" >/dev/null
CREATED_NETWORK=true
"$RUNTIME" volume create "$SQLITE_VOLUME" >/dev/null
CREATED_SQLITE_VOLUME=true
"$RUNTIME" volume create "$POSTGRES_VOLUME" >/dev/null
CREATED_POSTGRES_VOLUME=true
"$RUNTIME" run -d --name "$MOCK_CONTAINER" --network "$NETWORK" docker.io/library/python:3.13-alpine \
  python3 -c 'from http.server import BaseHTTPRequestHandler,HTTPServer
class H(BaseHTTPRequestHandler):
 def do_POST(self):
  ok=self.headers.get("Authorization")=="Bearer migration-e2e-real-secret"
  self.send_response(200 if ok else 401); self.send_header("Content-Type","application/json"); self.end_headers(); self.wfile.write(b"{\"credential_injected\":true}" if ok else b"{\"credential_injected\":false}")
 def log_message(self,*args): pass
HTTPServer(("0.0.0.0",8081),H).serve_forever()' >/dev/null
CREATED_MOCK_CONTAINER=true
"$RUNTIME" run -d --name "$SQLITE_CONTAINER" --network "$NETWORK" \
  -p 127.0.0.1::8080 -v "$SQLITE_VOLUME:/data" "$SERVER_IMAGE" >/dev/null
CREATED_SQLITE_CONTAINER=true
SQLITE_BASE="$(container_base_url "$SQLITE_CONTAINER")"
wait_for_url "$SQLITE_BASE/healthz" || { "$RUNTIME" logs "$SQLITE_CONTAINER"; exit 1; }
ADMIN_PASSWORD="$($RUNTIME logs "$SQLITE_CONTAINER" 2>&1 | sed -n 's/.*Password: //p' | tail -1)"
test -n "$ADMIN_PASSWORD"

curl -fsS -c "$COOKIE_FILE" -X POST "$SQLITE_BASE/api/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"duckway\",\"password\":\"$ADMIN_PASSWORD\"}" | jq -e '.status == "ok"' >/dev/null

echo "[3/7] Seeding a realistic SQLite dataset through Duckway APIs"
SERVICES="$(curl -fsS -b "$COOKIE_FILE" "$SQLITE_BASE/api/services")"
OPENAI_ID="$(jq -r '.[] | select(.name == "openai") | .id' <<<"$SERVICES")"
ANTHROPIC_ID="$(jq -r '.[] | select(.name == "anthropic") | .id' <<<"$SERVICES")"
MOCK_SERVICE="$(curl -fsS -b "$COOKIE_FILE" -X POST "$SQLITE_BASE/api/services" -H 'Content-Type: application/json' \
  -d "{\"name\":\"migrationmock\",\"display_name\":\"Migration Mock\",\"upstream_url\":\"http://$MOCK_CONTAINER:8081\",\"host_pattern\":\"migration-mock.invalid\",\"auth_type\":\"bearer\"}")"
MOCK_SERVICE_ID="$(jq -r .id <<<"$MOCK_SERVICE")"
KEY_ONE="$(curl -fsS -b "$COOKIE_FILE" -X POST "$SQLITE_BASE/api/keys" -H 'Content-Type: application/json' \
  -d "{\"service_id\":\"$OPENAI_ID\",\"name\":\"Migration OpenAI\",\"key\":\"sk-proj-migration-e2e-1234567890abcdef1234567890abcdef\"}")"
KEY_TWO="$(curl -fsS -b "$COOKIE_FILE" -X POST "$SQLITE_BASE/api/keys" -H 'Content-Type: application/json' \
  -d "{\"service_id\":\"$ANTHROPIC_ID\",\"name\":\"Migration Claude\",\"key\":\"sk-ant-migration-e2e-1234567890abcdef\"}")"
MOCK_KEY="$(curl -fsS -b "$COOKIE_FILE" -X POST "$SQLITE_BASE/api/keys" -H 'Content-Type: application/json' \
  -d "{\"service_id\":\"$MOCK_SERVICE_ID\",\"name\":\"Migration Encrypted Key\",\"key\":\"migration-e2e-real-secret\"}")"
CLIENT_ONE="$(curl -fsS -b "$COOKIE_FILE" -X POST "$SQLITE_BASE/api/clients" -H 'Content-Type: application/json' -d '{"name":"migration-client-one"}')"
CLIENT_TWO="$(curl -fsS -b "$COOKIE_FILE" -X POST "$SQLITE_BASE/api/clients" -H 'Content-Type: application/json' -d '{"name":"migration-client-two"}')"
KEY_ONE_ID="$(jq -r .id <<<"$KEY_ONE")"
KEY_TWO_ID="$(jq -r .id <<<"$KEY_TWO")"
MOCK_KEY_ID="$(jq -r .id <<<"$MOCK_KEY")"
CLIENT_ONE_ID="$(jq -r .id <<<"$CLIENT_ONE")"
CLIENT_TWO_ID="$(jq -r .id <<<"$CLIENT_TWO")"
CLIENT_ONE_TOKEN="$(jq -r .token <<<"$CLIENT_ONE")"
curl -fsS -b "$COOKIE_FILE" -X POST "$SQLITE_BASE/api/placeholders" -H 'Content-Type: application/json' \
  -d "{\"service_id\":\"$OPENAI_ID\",\"api_key_id\":\"$KEY_ONE_ID\",\"client_id\":\"$CLIENT_ONE_ID\",\"requires_approval\":false}" | jq -e '.id != null' >/dev/null
curl -fsS -b "$COOKIE_FILE" -X POST "$SQLITE_BASE/api/placeholders" -H 'Content-Type: application/json' \
  -d "{\"service_id\":\"$ANTHROPIC_ID\",\"api_key_id\":\"$KEY_TWO_ID\",\"client_id\":\"$CLIENT_TWO_ID\",\"requires_approval\":true}" | jq -e '.id != null' >/dev/null
curl -fsS -b "$COOKIE_FILE" -X POST "$SQLITE_BASE/api/placeholders" -H 'Content-Type: application/json' \
  -d "{\"service_id\":\"$MOCK_SERVICE_ID\",\"api_key_id\":\"$MOCK_KEY_ID\",\"client_id\":\"$CLIENT_ONE_ID\",\"requires_approval\":false}" | jq -e '.id != null' >/dev/null
curl -fsS -X POST -H "X-Duckway-Token: $CLIENT_ONE_TOKEN" "$SQLITE_BASE/proxy/migrationmock/credential-check" | jq -e '.credential_injected == true' >/dev/null

SQLITE_KEY_COUNT="$(curl -fsS -b "$COOKIE_FILE" "$SQLITE_BASE/api/keys" | jq length)"
SQLITE_CLIENT_COUNT="$(curl -fsS -b "$COOKIE_FILE" "$SQLITE_BASE/api/clients" | jq length)"
SQLITE_PLACEHOLDER_COUNT="$(curl -fsS -b "$COOKIE_FILE" "$SQLITE_BASE/api/placeholders" | jq length)"
test "$SQLITE_KEY_COUNT" -ge 3
test "$SQLITE_CLIENT_COUNT" -eq 2
test "$SQLITE_PLACEHOLDER_COUNT" -ge 2

echo "[4/7] Adding legacy bigint and foreign-key edge cases"
"$RUNTIME" stop "$SQLITE_CONTAINER" >/dev/null
"$RUNTIME" run --rm -v "$SQLITE_VOLUME:/data" docker.io/library/alpine:3.21 sh -c \
  "apk add --no-cache sqlite >/dev/null && sqlite3 /data/duckway.db \"PRAGMA foreign_keys=OFF; UPDATE api_keys SET expires_at=1788598308000 WHERE id='$KEY_ONE_ID'; INSERT INTO placeholder_keys (id,env_name,placeholder,service_id,api_key_id,client_id,requires_approval) VALUES ('legacy-orphan','ORPHAN_TOKEN','dw_orphan_migration','$OPENAI_ID','$KEY_ONE_ID','deleted-legacy-client',0); WITH RECURSIVE seq(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM seq WHERE x<$REQUEST_LOG_COUNT) INSERT INTO request_log (client_id,service_name,method,path,status_code) SELECT '$CLIENT_ONE_ID','migrationmock','POST','/bulk/'||x,200 FROM seq; UPDATE request_log SET client_id='deleted-legacy-client' WHERE id IN (SELECT id FROM request_log ORDER BY id LIMIT $ORPHAN_LOG_COUNT); INSERT INTO request_log_detail (log_id,response_body) SELECT id,CAST(X'62B563' AS TEXT) FROM request_log ORDER BY id DESC LIMIT 1;\""

echo "[5/7] Migrating the read-only SQLite volume to PostgreSQL 17"
"$RUNTIME" run -d --name "$POSTGRES_CONTAINER" --network "$NETWORK" \
  -e POSTGRES_PASSWORD="$PASSWORD" -e POSTGRES_DB=duckway -e POSTGRES_USER=duckway \
  -v "$POSTGRES_VOLUME:/var/lib/postgresql/data" docker.io/library/postgres:17-alpine >/dev/null
CREATED_POSTGRES_CONTAINER=true
for _ in $(seq 1 60); do
  "$RUNTIME" exec "$POSTGRES_CONTAINER" pg_isready -U duckway -d duckway >/dev/null 2>&1 && break
  sleep 1
done
"$RUNTIME" exec "$POSTGRES_CONTAINER" pg_isready -U duckway -d duckway >/dev/null
MIGRATOR_ARGS=(--sqlite-data /data)
if [ "$SKIP_REQUEST_LOGS" = true ]; then
  MIGRATOR_ARGS+=(--skip-request-logs)
fi
MIGRATION_OUTPUT="$($RUNTIME run --rm --network "$NETWORK" --user 65534:65534 \
  -v "$SQLITE_VOLUME:/data:ro" \
  -e "DUCKWAY_DATABASE_URL=postgres://duckway:$PASSWORD@$POSTGRES_CONTAINER:5432/duckway?sslmode=disable" \
  "$MIGRATOR_IMAGE" "${MIGRATOR_ARGS[@]}" 2>&1)"
printf '%s\n' "$MIGRATION_OUTPUT"
grep -q 'excluded 1 orphan row(s) from placeholder_keys' <<<"$MIGRATION_OUTPUT"
if [ "$SKIP_REQUEST_LOGS" = true ]; then
  grep -q "skipped $EXPECTED_REQUEST_LOG_COUNT request_log and 1 request_log_detail row(s)" <<<"$MIGRATION_OUTPUT"
else
  grep -q "preserved $ORPHAN_LOG_COUNT orphan row(s) in request_log by clearing nullable foreign keys" <<<"$MIGRATION_OUTPUT"
  grep -q 'normalized 1 invalid UTF-8 TEXT value(s) in request_log_detail' <<<"$MIGRATION_OUTPUT"
fi
grep -q 'SQLite to PostgreSQL migration completed and verified' <<<"$MIGRATION_OUTPUT"

echo "[6/7] Starting split Admin and Gateway on the migrated PostgreSQL database"
"$RUNTIME" run -d --name "$ADMIN_CONTAINER" --network "$NETWORK" -p 127.0.0.1::9090 \
  -v "$SQLITE_VOLUME:/data" -e DUCKWAY_DATABASE_DRIVER=postgres \
  -e "DUCKWAY_DATABASE_URL=postgres://duckway:$PASSWORD@$POSTGRES_CONTAINER:5432/duckway?sslmode=disable" \
  "$ADMIN_IMAGE" >/dev/null
CREATED_ADMIN_CONTAINER=true
"$RUNTIME" run -d --name "$GATEWAY_CONTAINER" --network "$NETWORK" -p 127.0.0.1::8080 \
  -v "$SQLITE_VOLUME:/data" -e DUCKWAY_DATABASE_DRIVER=postgres \
  -e "DUCKWAY_DATABASE_URL=postgres://duckway:$PASSWORD@$POSTGRES_CONTAINER:5432/duckway?sslmode=disable" \
  "$GATEWAY_IMAGE" >/dev/null
CREATED_GATEWAY_CONTAINER=true
ADMIN_PORT="$($RUNTIME port "$ADMIN_CONTAINER" 9090/tcp | tail -1)"
ADMIN_BASE="http://$ADMIN_PORT"
GATEWAY_BASE="$(container_base_url "$GATEWAY_CONTAINER")"
wait_for_url "$ADMIN_BASE/healthz" || { "$RUNTIME" logs "$ADMIN_CONTAINER"; exit 1; }
wait_for_url "$GATEWAY_BASE/healthz" || { "$RUNTIME" logs "$GATEWAY_CONTAINER"; exit 1; }
curl -fsS -c "$PG_COOKIE_FILE" -X POST "$ADMIN_BASE/api/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"duckway\",\"password\":\"$ADMIN_PASSWORD\"}" | jq -e '.status == "ok"' >/dev/null

echo "[7/7] Verifying migrated data through Duckway and PostgreSQL"
PG_SERVICES="$(curl -fsS -b "$PG_COOKIE_FILE" "$ADMIN_BASE/api/services")"
PG_KEYS="$(curl -fsS -b "$PG_COOKIE_FILE" "$ADMIN_BASE/api/keys")"
PG_CLIENTS="$(curl -fsS -b "$PG_COOKIE_FILE" "$ADMIN_BASE/api/clients")"
PG_PLACEHOLDERS="$(curl -fsS -b "$PG_COOKIE_FILE" "$ADMIN_BASE/api/placeholders")"
test "$(jq length <<<"$PG_SERVICES")" -eq "$(( $(jq length <<<"$SERVICES") + 1 ))"
test "$(jq length <<<"$PG_KEYS")" -eq "$SQLITE_KEY_COUNT"
test "$(jq length <<<"$PG_CLIENTS")" -eq "$SQLITE_CLIENT_COUNT"
test "$(jq length <<<"$PG_PLACEHOLDERS")" -eq "$SQLITE_PLACEHOLDER_COUNT"
jq -e --arg id "$KEY_ONE_ID" '.[] | select(.id == $id and .expires_at == 1788598308000)' <<<"$PG_KEYS" >/dev/null
jq -e --arg id "$CLIENT_ONE_ID" '.[] | select(.id == $id and .name == "migration-client-one")' <<<"$PG_CLIENTS" >/dev/null
! jq -e '.[] | select(.id == "legacy-orphan")' <<<"$PG_PLACEHOLDERS" >/dev/null
if [ "$SKIP_REQUEST_LOGS" = true ]; then
  test "$($RUNTIME exec "$POSTGRES_CONTAINER" psql -U duckway -d duckway -Atc 'SELECT COUNT(*) FROM request_log')" -eq 0
  test "$($RUNTIME exec "$POSTGRES_CONTAINER" psql -U duckway -d duckway -Atc 'SELECT COUNT(*) FROM request_log_detail')" -eq 0
fi
curl -fsS -X POST -H "X-Duckway-Token: $CLIENT_ONE_TOKEN" "$GATEWAY_BASE/proxy/migrationmock/credential-check" | jq -e '.credential_injected == true' >/dev/null
curl -fsS -H "X-Duckway-Token: $CLIENT_ONE_TOKEN" "$GATEWAY_BASE/client/keys" | jq -e 'length > 0' >/dev/null
test "$(curl -s -o /dev/null -w '%{http_code}' "$GATEWAY_BASE/admin/")" = 404
test "$(curl -s -o /dev/null -w '%{http_code}' "$ADMIN_BASE/proxy/heartbeat/ping")" = 404
PG_EXPIRY="$($RUNTIME exec "$POSTGRES_CONTAINER" psql -U duckway -d duckway -Atc "SELECT expires_at FROM api_keys WHERE id='$KEY_ONE_ID'")"
test "$PG_EXPIRY" = 1788598308000
if [ "$SKIP_REQUEST_LOGS" != true ]; then
  test "$($RUNTIME exec "$POSTGRES_CONTAINER" psql -U duckway -d duckway -Atc 'SELECT COUNT(*) FROM request_log WHERE client_id IS NULL')" -eq "$ORPHAN_LOG_COUNT"
  test "$($RUNTIME exec "$POSTGRES_CONTAINER" psql -U duckway -d duckway -Atc "SELECT encode(convert_to(response_body,'UTF8'),'hex') FROM request_log_detail LIMIT 1")" = 62efbfbd63
fi

NEW_CLIENT="$(curl -fsS -b "$PG_COOKIE_FILE" -X POST "$ADMIN_BASE/api/clients" -H 'Content-Type: application/json' -d '{"name":"postgres-write-check"}')"
jq -e '.id != null and .token != null' <<<"$NEW_CLIENT" >/dev/null
test "$(curl -fsS -b "$PG_COOKIE_FILE" "$ADMIN_BASE/api/clients" | jq length)" -eq "$((SQLITE_CLIENT_COUNT + 1))"

set +e
SECOND_OUTPUT="$($RUNTIME run --rm --network "$NETWORK" --user 65534:65534 \
  -v "$SQLITE_VOLUME:/data:ro" \
  -e "DUCKWAY_DATABASE_URL=postgres://duckway:$PASSWORD@$POSTGRES_CONTAINER:5432/duckway?sslmode=disable" \
  "$MIGRATOR_IMAGE" "${MIGRATOR_ARGS[@]}" 2>&1)"
SECOND_STATUS=$?
set -e
test "$SECOND_STATUS" -ne 0
grep -q 'already contains' <<<"$SECOND_OUTPUT"
test "$($RUNTIME exec "$POSTGRES_CONTAINER" psql -U duckway -d duckway -Atc 'SELECT COUNT(*) FROM clients')" -eq "$((SQLITE_CLIENT_COUNT + 1))"

ORIGINAL_ORPHANS="$($RUNTIME run --rm -v "$SQLITE_VOLUME:/data:ro" docker.io/library/alpine:3.21 sh -c \
  "apk add --no-cache sqlite >/dev/null && sqlite3 /data/duckway.db \"SELECT COUNT(*) FROM placeholder_keys WHERE id='legacy-orphan';\"")"
test "$ORIGINAL_ORPHANS" = 1

echo "PASS: Podman Duckway SQLite -> PostgreSQL migration completed end to end"
echo "services=$(jq length <<<"$PG_SERVICES") keys=$(jq length <<<"$PG_KEYS") clients=$(jq length <<<"$PG_CLIENTS") placeholders=$(jq length <<<"$PG_PLACEHOLDERS") expiry=$PG_EXPIRY encrypted_proxy=ok second_migration=refused original_sqlite=unchanged"
