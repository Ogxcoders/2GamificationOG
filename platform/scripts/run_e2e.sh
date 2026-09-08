#!/usr/bin/env bash
# Full-stack E2E in ONE process lifetime (the sandbox reaps background
# processes between commands, so everything runs within this script).
# Boots: PostgreSQL → engine-service → control-plane → golden path assertions.
set -u

PGWRAP=$HOME/toolchains/pgwrap
PGDATA=/tmp/pgdata
PLATFORM=/home/z/my-project/platform
PASS=0; FAIL=0

say()  { echo "» $*"; }
ok()   { PASS=$((PASS+1)); echo "  ✓ $1"; }
bad()  { FAIL=$((FAIL+1)); echo "  ✗ $1"; echo "      $2" | head -c 400; echo; }
need() { if echo "$2" | grep -q "$3"; then ok "$1"; else bad "$1" "expected '$3' in: $2"; fi; }

# ── 0. Cleanup: kill anything left on our ports ──────────────────────────────
pkill -f 'control-plane' 2>/dev/null; pkill -f 'engine-service' 2>/dev/null
pkill -f 'postgres.real' 2>/dev/null; pkill -f 'postgres -D' 2>/dev/null
sleep 1

# ── 1. PostgreSQL ────────────────────────────────────────────────────────────
export PATH=$PGWRAP:$PATH
if ! pg_isready -h 127.0.0.1 -p 5432 >/dev/null 2>&1; then
  say "starting PostgreSQL"
  if [ ! -f $PGDATA/PG_VERSION ]; then
    rm -rf $PGDATA && mkdir -p $PGDATA && chmod 700 $PGDATA
    initdb -D $PGDATA -U postgres --auth=trust -E UTF8 >/dev/null 2>&1 || { echo "initdb failed"; exit 1; }
    printf "listen_addresses = '127.0.0.1'\nport = 5432\nunix_socket_directories = '/tmp'\n" >> $PGDATA/postgresql.conf
  fi
  postgres -D $PGDATA > /tmp/pg.log 2>&1 &
  for i in $(seq 1 20); do pg_isready -h 127.0.0.1 -p 5432 >/dev/null 2>&1 && break; sleep 0.5; done
fi
pg_isready -h 127.0.0.1 -p 5432 >/dev/null 2>&1 && ok "postgres up" || { bad "postgres up" "$(tail -3 /tmp/pg.log)"; exit 1; }
# Hard reset of the platform DB (terminate connections, drop, recreate).
# NOTE: must target the `platform` database explicitly — psql defaults to the
# `postgres` DB and would otherwise reset the WRONG database, leaving stale
# tenants behind (breaks the first-run bootstrap guard).
psql -h 127.0.0.1 -U postgres -d postgres -c "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='platform' AND pid <> pg_backend_pid();" >/dev/null 2>&1
psql -h 127.0.0.1 -U postgres -d postgres -c "DROP DATABASE IF EXISTS platform;" >/dev/null 2>&1
psql -h 127.0.0.1 -U postgres -d postgres -c "CREATE DATABASE platform;" >/dev/null 2>&1
psql -h 127.0.0.1 -U postgres -d platform -tc "SELECT 1" | grep -q 1 && ok "platform database recreated" || { bad "platform database" "create failed"; exit 1; }

# ── 2. engine-service ────────────────────────────────────────────────────────
say "starting engine-service"
ENGINE_BIND=127.0.0.1:8081 RUST_LOG=warn $PLATFORM/target/debug/engine-service > /tmp/engine.log 2>&1 &
for i in $(seq 1 30); do curl -fsS http://127.0.0.1:8081/healthz >/dev/null 2>&1 && break; sleep 0.5; done
curl -fsS http://127.0.0.1:8081/healthz >/dev/null 2>&1 && ok "engine-service up" || { bad "engine-service up" "$(tail -3 /tmp/engine.log)"; }

# ── 3. control-plane ─────────────────────────────────────────────────────────
say "starting control-plane (migrations run at boot)"
DATABASE_URL="postgres://postgres:postgres@127.0.0.1:5432/platform?sslmode=disable" \
ENGINE_URL="http://127.0.0.1:8081" \
CONTROL_PLANE_BIND="127.0.0.1:8080" \
/tmp/control-plane > /tmp/cp.log 2>&1 &
for i in $(seq 1 30); do curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1 && break; sleep 0.5; done
curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1 && ok "control-plane up" || { bad "control-plane up" "$(tail -3 /tmp/cp.log)"; exit 1; }

BASE=http://127.0.0.1:8080
PY=python3

# ── 4. Security baseline ─────────────────────────────────────────────────────
say "security: unauthenticated requests must be rejected"
CODE=$(curl -sS -o /dev/null -w '%{http_code}' $BASE/v1/projects)
need "401 without key" "$CODE" "401"
CODE=$(curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer junk" $BASE/v1/projects)
need "401 with unknown key" "$CODE" "401"
CODE=$(curl -sS -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{}' $BASE/v1/projects/e1/environments/d/objects)
need "401 on write route too" "$CODE" "401"

# ── 5. Bootstrap (Customer Zero step 1) ──────────────────────────────────────
say "bootstrap tenant"
BOOT=$(curl -fsS -X POST $BASE/v1/bootstrap -H 'Content-Type: application/json' -d '{"organization":"E2E Corp","project":"e2e-project"}')
KEY=$(echo "$BOOT" | $PY -c 'import json,sys; print(json.load(sys.stdin)["api_key"])')
PROJ=$(echo "$BOOT" | $PY -c 'import json,sys; print(json.load(sys.stdin)["project"]["id"])')
AUTH="Authorization: Bearer $KEY"
SCOPE="$BASE/v1/projects/$PROJ/environments/development"
[ -n "$KEY" ] && ok "bootstrap returned key + scope" || bad "bootstrap" "$BOOT"

# bootstrap must be guarded on the second call (no token)
CODE=$(curl -sS -o /dev/null -w '%{http_code}' -X POST $BASE/v1/bootstrap -H 'Content-Type: application/json' -d '{"organization":"X","project":"Y"}')
[ "$CODE" = "401" ] && ok "bootstrap guarded after first run" || bad "bootstrap guard" "got $CODE"

# ── 6. Configure: create + publish fixture objects ──────────────────────────
say "create + publish golden-path objects"
OUT=$($PY << PYEOF
import json, urllib.request, sys
base = "$SCOPE"
bearer = "$KEY"
fx = json.load(open("$PLATFORM/packages/testing/fixtures/golden-path.json"))
ids = {}
for obj in fx["objects"]:
    body = json.dumps({"name": obj["name"], "type": obj["type"], "config": obj["config"]}).encode()
    req = urllib.request.Request(f"{base}/objects", data=body, method="POST")
    req.add_header("Content-Type", "application/json")
    req.add_header("Authorization", "Bearer " + bearer)
    with urllib.request.urlopen(req) as r:
        oid = json.load(r)["id"]
    ids[obj["name"]] = oid
    req = urllib.request.Request(f"{base}/objects/{oid}/publish", data=b"{}", method="POST")
    req.add_header("Authorization", "Bearer " + bearer)
    urllib.request.urlopen(req).read()
print(json.dumps(ids))
PYEOF
)
echo "$OUT" | grep -q lesson-completed-xp && ok "7 objects created + published" || bad "objects" "$OUT"

# Validation gate: a malformed rule must be rejected at CREATE (never enters pipeline)
CODE=$(curl -sS -o /tmp/val.json -w '%{http_code}' -X POST $SCOPE/objects -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"name":"bad-rule","type":"rule","config":{"event_type":"x.y"}}')
need "malformed rule rejected at create (422)" "$CODE" "422"

# Published object immutability
RULE_ID=$(echo "$OUT" | $PY -c 'import json,sys; print(json.load(sys.stdin)["lesson-completed-xp"])')
CODE=$(curl -sS -o /dev/null -w '%{http_code}' -X PUT $SCOPE/objects/$RULE_ID -H "$AUTH" -H 'Content-Type: application/json' -d '{"name":"x","type":"rule","config":{}}')
need "published object immutable (409)" "$CODE" "409"

# ── 7. Ingest events ─────────────────────────────────────────────────────────
say "ingest + duplicate replay"
EVENTS=$($PY -c 'import json,sys; print(json.dumps({"events": json.load(open(sys.argv[1]))["events"]}))' $PLATFORM/packages/testing/fixtures/golden-path.json)
ING=$(curl -fsS -X POST $SCOPE/events -H "$AUTH" -H 'Content-Type: application/json' -d "$EVENTS")
need "5 events inserted" "$ING" '"inserted":5'
DUP=$(curl -fsS -X POST $SCOPE/events -H "$AUTH" -H 'Content-Type: application/json' -d "$EVENTS")
need "replay deduped truthfully" "$DUP" '"duplicates":5'

# Malformed event rejected
CODE=$(curl -sS -o /dev/null -w '%{http_code}' -X POST $SCOPE/events -H "$AUTH" -H 'Content-Type: application/json' -d '{"events":[{"event_type":"lesson.completed"}]}')
need "event without actor rejected (422)" "$CODE" "422"

# ── 8. Process ───────────────────────────────────────────────────────────────
say "processing pipeline"
IDS=$(echo "$ING" | $PY -c 'import json,sys; print(" ".join(r["event_id"] for r in json.load(sys.stdin)["results"]))')
for EID in $IDS; do
  RES=$(curl -sS -X POST $SCOPE/events/$EID/process -H "$AUTH")
  if echo "$RES" | grep -q '"processed":true'; then ok "processed $EID"; else bad "process $EID" "$RES"; fi
done

# ── 9. Player state verification ─────────────────────────────────────────────
say "player state assertions"
ST=$(curl -fsS $SCOPE/users/user-alice/state -H "$AUTH")
need "alice xp=300" "$ST" '"xp":300'
need "alice level=3" "$ST" '"level":3'
need "alice coins=65 (3×5 + 50 challenge)" "$ST" '"balance":65'
need "challenge completed" "$ST" '"status":"completed"'
need "streak day 1" "$ST" '"current":1'
STB=$(curl -fsS $SCOPE/users/user-bob/state -H "$AUTH")
need "bob xp=100" "$STB" '"xp":100'

# Re-processing the same event must not double-award (idempotency)
FIRST=$(echo $IDS | awk '{print $1}')
R1=$(curl -sS -X POST $SCOPE/events/$FIRST/process -H "$AUTH")
R1_APPLIED=$(echo "$R1" | $PY -c 'import json,sys; print(json.load(sys.stdin).get("applied_commands", -1))')
[ "$R1_APPLIED" = "0" ] && ok "replay process: 0 commands applied" || bad "replay idempotency" "$R1"
ST2=$(curl -fsS $SCOPE/users/user-alice/state -H "$AUTH")
need "alice xp still 300 after replay" "$ST2" '"xp":300'

# ── 10. Trace: WHY ───────────────────────────────────────────────────────────
say "decision trace"
TR=$(curl -fsS $SCOPE/events/$FIRST/trace -H "$AUTH")
need "trace records rule_matched" "$TR" 'rule_matched'
need "trace records ledger entries" "$TR" 'ledger_entry'

# ── 11. Leaderboard ──────────────────────────────────────────────────────────
say "leaderboard"
LBID=$(curl -fsS "$SCOPE/objects?type=leaderboard" -H "$AUTH" | $PY -c 'import json,sys; print(json.load(sys.stdin)["objects"][0]["id"])')
LB=$(curl -fsS "$SCOPE/leaderboards/$LBID?limit=10" -H "$AUTH")
need "alice ranks #1 (300)" "$LB" '"rank":1'
need "bob ranks #2 (100)" "$LB" '"rank":2'

# ── 12. Economy guards ───────────────────────────────────────────────────────
say "economy NET validation"
SPEND=$(curl -sS -X POST $SCOPE/users/user-bob/wallets/adjust -H "$AUTH" -H 'Content-Type: application/json' -d '{"currency":"coin","amount":-999,"reference":"overspend-1"}')
need "overspend refused" "$SPEND" 'insufficient'
# Idempotent adjust
A1=$(curl -fsS -X POST $SCOPE/users/user-alice/wallets/adjust -H "$AUTH" -H 'Content-Type: application/json' -d '{"currency":"coin","amount":10,"reference":"grant-1"}')
A2=$(curl -fsS -X POST $SCOPE/users/user-alice/wallets/adjust -H "$AUTH" -H 'Content-Type: application/json' -d '{"currency":"coin","amount":10,"reference":"grant-1"}')
need "wallet adjust applied once" "$A1" '"applied":true'
need "wallet replay not applied" "$A2" '"applied":false'
need "balance still 75 (65+10)" "$A2" '"balance":75'

# ── 13. Config lifecycle: pause stops firing ─────────────────────────────────
say "pause/resume semantics"
curl -fsS -X POST $SCOPE/objects/$RULE_ID/pause -H "$AUTH" > /dev/null
EV2=$(curl -fsS -X POST $SCOPE/events -H "$AUTH" -H 'Content-Type: application/json' -d '{"events":[{"event_type":"lesson.completed","actor_id":"user-alice","payload":{"lesson_id":"l9"}}]}')
EID2=$(echo "$EV2" | $PY -c 'import json,sys; print(json.load(sys.stdin)["results"][0]["event_id"])')
P2=$(curl -sS -X POST $SCOPE/events/$EID2/process -H "$AUTH")
ST3=$(curl -fsS $SCOPE/users/user-alice/state -H "$AUTH")
need "paused rule: no xp change" "$ST3" '"xp":300'
curl -fsS -X POST $SCOPE/objects/$RULE_ID/resume -H "$AUTH" > /dev/null

# ── 14. Cross-tenant isolation ───────────────────────────────────────────────
say "tenant isolation"
# A scoped key for the SAME project must 404 other tenants; make a second tenant
BOOT2=$(curl -sS -X POST $BASE/v1/bootstrap -H 'Content-Type: application/json' -H "$AUTH" -d '{"organization":"Other Corp","project":"other-project"}')
if echo "$BOOT2" | grep -q '"api_key"'; then
  KEY2=$(echo "$BOOT2" | $PY -c 'import json,sys; print(json.load(sys.stdin)["api_key"])')
  CODE=$(curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $KEY2" $SCOPE/users/user-alice/state)
  need "scoped key: other tenant 404" "$CODE" "404"
else
  # Bootstrap with an existing key requires org key scope; fall back to scope-check with same key
  CODE=$(curl -sS -o /dev/null -w '%{http_code}' -H "$AUTH" $BASE/v1/projects/nonexistent-project/environments/development/users/x/state)
  need "nonexistent project 404 (no existence leak)" "$CODE" "404"
fi

# ── 15. MCP tools ────────────────────────────────────────────────────────────
say "MCP JSON-RPC"
TOOLS=$(curl -fsS -X POST $SCOPE/mcp -H "$AUTH" -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}')
need "tools/list works" "$TOOLS" 'list_config_objects'
EXPLAIN=$(curl -fsS -X POST $SCOPE/mcp -H "$AUTH" -H 'Content-Type: application/json' -d "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"explain_event\",\"arguments\":{\"event_id\":\"$FIRST\"}}}")
need "explain_event returns trace" "$EXPLAIN" 'rule_matched'
PLST=$(curl -fsS -X POST $SCOPE/mcp -H "$AUTH" -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"player_state","arguments":{"user_id":"user-alice"}}}')
need "player_state tool works" "$PLST" '"xp\\":300'

# ── 16. Webhooks ─────────────────────────────────────────────────────────────
say "webhooks"
WH=$(curl -fsS -X POST $SCOPE/webhooks -H "$AUTH" -H 'Content-Type: application/json' -d '{"url":"https://example.test/hook","events":["event.processed"]}')
need "webhook created with secret" "$WH" '"secret"'

# ── 17. Analytics ────────────────────────────────────────────────────────────
say "analytics"
AN=$(curl -fsS "$SCOPE/analytics/overview?days=1" -H "$AUTH")
need "analytics events_by_day" "$AN" 'events_by_day'
need "analytics DAU" "$AN" '"dau"'

# ── 18. Simulator ────────────────────────────────────────────────────────────
say "simulator"
SIM=$(curl -fsS -X POST $SCOPE/simulate -H "$AUTH" -H 'Content-Type: application/json' -d '{"seed":42,"users":10,"days":2}')
echo "$SIM" | grep -q "report\|sample" && ok "simulation ran" || bad "simulation" "$SIM"

echo
echo "════ E2E RESULT: $PASS passed, $FAIL failed ════"
[ "$FAIL" -eq 0 ]
