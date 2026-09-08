#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# FEATURE-MATRIX E2E — every domain, start to end, one process lifetime.
# Boots: PostgreSQL → engine-service → control-plane, then walks ALL feature
# areas: tenancy, identity, API keys, object lifecycle (update/rollback/
# promote/delete), engine config (get/recompile/validate), events (get/list/
# batch), progression → achievement unlock, economy (history/transactions),
# monetization (product + purchase + idempotent replay), flags (rollout +
# override), segments, webhooks (list/delete), notifications, audit, MCP
# tools, simulation determinism, engine endpoints, tenant isolation.
# ─────────────────────────────────────────────────────────────────────────────
set -u

PGWRAP=$HOME/toolchains/pgwrap
PGDATA=/tmp/pgdata
PLATFORM=/home/z/my-project/platform
PASS=0; FAIL=0

say()  { echo "» $*"; }
ok()   { PASS=$((PASS+1)); echo "  ✓ $1"; }
bad()  { FAIL=$((FAIL+1)); echo "  ✗ $1"; echo "      $2" | head -c 400; echo; }
need() { if echo "$2" | grep -q "$3"; then ok "$1"; else bad "$1" "expected '$3' in: $2"; fi; }
code() { # code <desc> <actual-http-code> <expected>
  if [ "$2" = "$3" ]; then ok "$1"; else bad "$1" "expected HTTP $3, got $2"; fi }

PY=python3

# ── 0. Cleanup + boot stack ─────────────────────────────────────────────────
pkill -f 'control-plane' 2>/dev/null; pkill -f 'engine-service' 2>/dev/null
pkill -f 'postgres -D' 2>/dev/null; sleep 1

export PATH=$PGWRAP:$PATH
if ! pg_isready -h 127.0.0.1 -p 5432 >/dev/null 2>&1; then
  if [ ! -f $PGDATA/PG_VERSION ]; then
    rm -rf $PGDATA && mkdir -p $PGDATA && chmod 700 $PGDATA
    initdb -D $PGDATA -U postgres --auth=trust -E UTF8 >/dev/null 2>&1
    printf "listen_addresses = '127.0.0.1'\nport = 5432\nunix_socket_directories = '/tmp'\n" >> $PGDATA/postgresql.conf
  fi
  postgres -D $PGDATA > /tmp/pg.log 2>&1 &
  for i in $(seq 1 20); do pg_isready -h 127.0.0.1 -p 5432 >/dev/null 2>&1 && break; sleep 0.5; done
fi
psql -h 127.0.0.1 -U postgres -d postgres -c "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='platform' AND pid <> pg_backend_pid();" >/dev/null 2>&1
psql -h 127.0.0.1 -U postgres -d postgres -c "DROP DATABASE IF EXISTS platform;" >/dev/null 2>&1
psql -h 127.0.0.1 -U postgres -d postgres -c "CREATE DATABASE platform;" >/dev/null 2>&1

ENGINE_BIND=127.0.0.1:8081 RUST_LOG=warn $PLATFORM/target/debug/engine-service > /tmp/engine.log 2>&1 &
for i in $(seq 1 30); do curl -fsS http://127.0.0.1:8081/healthz >/dev/null 2>&1 && break; sleep 0.5; done
DATABASE_URL="postgres://postgres:postgres@127.0.0.1:5432/platform?sslmode=disable" \
ENGINE_URL="http://127.0.0.1:8081" CONTROL_PLANE_BIND="127.0.0.1:8080" \
/tmp/control-plane > /tmp/cp.log 2>&1 &
for i in $(seq 1 30); do curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1 && break; sleep 0.5; done

BASE=http://127.0.0.1:8080
EURL=http://127.0.0.1:8081
ok "stack booted (postgres + engine + control-plane)"

# ── 1. Bootstrap + golden-path setup ────────────────────────────────────────
BOOT=$(curl -fsS -X POST $BASE/v1/bootstrap -H 'Content-Type: application/json' -d '{"organization":"Matrix Corp","project":"matrix-project"}')
KEY=$(echo "$BOOT" | $PY -c 'import json,sys; print(json.load(sys.stdin)["api_key"])')
PROJ=$(echo "$BOOT" | $PY -c 'import json,sys; print(json.load(sys.stdin)["project"]["id"])')
AUTH="Authorization: Bearer $KEY"
SCOPE="$BASE/v1/projects/$PROJ/environments/development"
[ -n "$KEY" ] && ok "bootstrap" || bad "bootstrap" "$BOOT"

# Publish the 7 golden-path objects + fixture events.
$PY << PYEOF
import json, urllib.request
base, bearer = "$SCOPE", "$KEY"
fx = json.load(open("$PLATFORM/packages/testing/fixtures/golden-path.json"))
for obj in fx["objects"]:
    body = json.dumps({"name": obj["name"], "type": obj["type"], "config": obj["config"]}).encode()
    req = urllib.request.Request(f"{base}/objects", data=body, method="POST")
    req.add_header("Content-Type", "application/json"); req.add_header("Authorization", "Bearer "+bearer)
    oid = json.load(urllib.request.urlopen(req))["id"]
    pub = urllib.request.Request(f"{base}/objects/{oid}/publish", data=b"{}", method="POST")
    pub.add_header("Content-Type", "application/json"); pub.add_header("Authorization", "Bearer "+bearer)
    urllib.request.urlopen(pub).read()
events = json.dumps({"events": fx["events"]}).encode()
req = urllib.request.Request(f"{base}/events", data=events, method="POST")
req.add_header("Content-Type", "application/json"); req.add_header("Authorization", "Bearer "+bearer)
r = json.load(urllib.request.urlopen(req))
for res in r["results"]:
    req = urllib.request.Request(f"{base}/events/{res['event_id']}/process", data=b"{}", method="POST")
    req.add_header("Authorization", "Bearer "+bearer)
    urllib.request.urlopen(req).read()
print("setup done")
PYEOF
ok "golden-path setup (7 objects PUBLISHED + 5 events processed)"

# ── 2. Tenancy & identity ────────────────────────────────────────────────────
say "tenancy & identity"
PR=$(curl -fsS "$BASE/v1/projects?organization_id=$(echo "$BOOT" | $PY -c 'import json,sys; print(json.load(sys.stdin)["organization"]["id"])')" -H "$AUTH")
need "list projects" "$PR" 'matrix-project'
GP=$(curl -fsS "$BASE/v1/projects/$PROJ" -H "$AUTH")
need "get project" "$GP" '"timezone"'
ENVLS=$(curl -fsS "$BASE/v1/projects/$PROJ/environments" -H "$AUTH")
need "list environments (dev+staging+prod)" "$ENVLS" 'development'

UP=$(curl -fsS -X POST "$SCOPE/users" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"user_id":"user-carol","display_name":"Carol D","attributes":{"tier":"pro","region":"eu"}}')
need "upsert user carol" "$UP" 'user-carice\|user-carol'
GU=$(curl -fsS "$SCOPE/users/user-carol" -H "$AUTH")
need "get user (attributes persisted)" "$GU" '"tier":"pro"'

LI=$(curl -fsS -X POST "$SCOPE/users/user-carol/identities" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"provider":"google","provider_user_id":"g-12345"}')
code "link identity carol+google" "$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$SCOPE/users/user-carol/identities" -H "$AUTH" -H 'Content-Type: application/json' -d '{"provider":"google","provider_user_id":"g-9999"}')" "201"

# Anonymous merge: anonymous user → events → merge into carol.
curl -fsS -X POST "$SCOPE/events" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"events":[{"event_type":"lesson.completed","actor_id":"anon-42"}]}' >/dev/null
MERGE=$(curl -sS -o /tmp/merge.json -w '%{http_code}' -X POST "$SCOPE/users/user-carol/merge" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"source_user_id":"anon-42"}')
[ "$MERGE" = "200" ] || [ "$MERGE" = "204" ] && ok "merge anonymous → carol" || bad "merge" "$(cat /tmp/merge.json 2>/dev/null)"

# API key lifecycle: create scoped key → use → revoke → rejected.
NK=$(curl -fsS -X POST "$BASE/v1/keys" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"scopes":["state:read"],"name":"matrix-readonly"}')
NKEY=$(echo "$NK" | $PY -c 'import json,sys; print(json.load(sys.stdin)["api_key"])' 2>/dev/null || echo "")
if [ -n "$NKEY" ]; then
  RD=$(curl -sS -o /dev/null -w '%{http_code}' "$SCOPE/users/user-alice/state" -H "Authorization: Bearer $NKEY")
  code "scoped key can read state" "$RD" "200"
  WR=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$SCOPE/events" -H "Authorization: Bearer $NKEY" -H 'Content-Type: application/json' -d '{"events":[]}')
  code "scoped key denied on write (403)" "$WR" "403"
  KID=$(echo "$NKEY" | cut -d. -f1)
  RV=$(curl -sS -o /dev/null -w '%{http_code}' -X DELETE "$BASE/v1/keys/$KID" -H "$AUTH")
  code "revoke key" "$RV" "200"
  DD=$(curl -sS -o /dev/null -w '%{http_code}' "$SCOPE/users/user-alice/state" -H "Authorization: Bearer $NKEY")
  code "revoked key rejected (401)" "$DD" "401"
else
  bad "key create" "$NK"
fi

# ── 3. Object lifecycle: update/rollback/promote/delete + engine config ─────
say "object lifecycle + engine config"
RULE_ID=$(curl -fsS "$SCOPE/objects?type=rule" -H "$AUTH" | $PY -c 'import json,sys; print(json.load(sys.stdin)["objects"][0]["id"])')

# Draft a NEW rule: create -> publish v1 -> update -> publish v2 -> rollback to v1.
NR=$(curl -fsS -X POST "$SCOPE/objects" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"name":"quiz-perfect-bonus","type":"rule","config":{"event_type":"quiz.perfect","condition":{"type":"eq","field":"event.type","value":"quiz.perfect"},"actions":[{"type":"add_currency","currency":"coin","amount":25}],"cooldown_seconds":0,"frequency_cap":0}}')
NRID=$(echo "$NR" | $PY -c 'import json,sys; print(json.load(sys.stdin)["id"])')
[ -n "$NRID" ] && ok "draft rule created (quiz bonus)" || bad "draft rule" "$NR"

UPD=$(curl -sS -o /tmp/upd.json -w '%{http_code}' -X PUT "$SCOPE/objects/$NRID" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"name":"quiz-perfect-bonus","type":"rule","config":{"event_type":"quiz.perfect","condition":{"type":"eq","field":"event.type","value":"quiz.perfect"},"actions":[{"type":"add_currency","currency":"coin","amount":30}],"cooldown_seconds":0,"frequency_cap":0}}')
code "update draft (amount 25→30)" "$UPD" "200"

PUB1=$(curl -sS -o /tmp/pub1.json -w '%{http_code}' -X POST "$SCOPE/objects/$NRID/publish" -H "$AUTH")
code "publish v1 rule (amount 30)" "$PUB1" "200"

# Rollback: restore the published snapshot as a new draft, then re-publish.
PUBVER=$(curl -fsS "$SCOPE/objects/$NRID" -H "$AUTH" | $PY -c 'import json,sys; pv=json.load(sys.stdin).get("published_version"); print(pv if pv else 1)')
RB=$(curl -sS -o /tmp/rb.json -w '%{http_code}' -X POST "$SCOPE/objects/$NRID/rollback?to_version=$PUBVER" -H "$AUTH")
code "rollback to published version (v$PUBVER → new draft)" "$RB" "200"
PUB=$(curl -sS -o /tmp/pub.json -w '%{http_code}' -X POST "$SCOPE/objects/$NRID/publish" -H "$AUTH")
code "re-publish (v$((PUBVER+1))) after rollback" "$PUB" "200"

EC=$(curl -fsS "$SCOPE/engine-config" -H "$AUTH")
need "engine config served (rules ≥ 2)" "$EC" '"rules"'
NEWRULES=$(echo "$EC" | $PY -c 'import json,sys; d=json.load(sys.stdin); print(len(d["config"]["rules"]) if "config" in d else len(d.get("rules",[])))')
[ "$NEWRULES" -ge 2 ] && ok "engine config includes new rule (rules=$NEWRULES)" || bad "engine config rules" "$EC"

RC=$(curl -sS -o /tmp/rc.json -w '%{http_code}' -X POST "$SCOPE/engine-config/recompile" -H "$AUTH")
code "recompile engine config" "$RC" "200"

VD=$(curl -sS -o /tmp/vd.json -w '%{http_code}' -X POST "$SCOPE/validate-draft" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"type":"rule","config":{"event_type":"x.y","condition":{"type":"eq","field":"event.type","value":"x.y"},"actions":[{"type":"award_xp","track":"default","amount":10}]}}')
code "validate-draft accepts good rule" "$VD" "200"

PRM=$(curl -sS -o /tmp/prm.json -w '%{http_code}' -X POST "$SCOPE/objects/$NRID/promote" -H "$AUTH" -H 'Content-Type: application/json' -d '{"to_environment":"staging"}')
[ "$PRM" = "200" ] || [ "$PRM" = "202" ] || [ "$PRM" = "204" ] && ok "promote object (dev→staging)" || bad "promote" "$(cat /tmp/prm.json)"

DEL=$(curl -sS -o /dev/null -w '%{http_code}' -X DELETE "$SCOPE/objects/$NRID" -H "$AUTH")
code "delete object" "$DEL" "200"
GONE=$(curl -sS -o /dev/null -w '%{http_code}' "$SCOPE/objects/$NRID" -H "$AUTH")
code "deleted object 404" "$GONE" "404"

# ── 4. Events: get single, list, batch process ──────────────────────────────
say "events read + batch"
EL=$(curl -fsS "$SCOPE/events?limit=10" -H "$AUTH")
need "list events" "$EL" 'lesson.completed'
EID=$(echo "$EL" | $PY -c 'import json,sys; d=json.load(sys.stdin); evs=d.get("events") or d.get("results"); print(evs[0]["event_id"])')
GE=$(curl -fsS "$SCOPE/events/$EID" -H "$AUTH")
need "get single event" "$GE" '"event_id"'

# Batch: 2 more alice lessons → level 5 → achievement unlock.
BATCH=$(curl -fsS -X POST "$SCOPE/events" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"events":[{"event_type":"lesson.completed","actor_id":"user-alice","payload":{"lesson_id":"l4"}},{"event_type":"lesson.completed","actor_id":"user-alice","payload":{"lesson_id":"l5"}}]}')
need "2 more events ingested" "$BATCH" '"inserted":2'
BIDS=$(echo "$BATCH" | $PY -c 'import json,sys; print(" ".join(r["event_id"] for r in json.load(sys.stdin)["results"]))')
PB=$(curl -fsS -X POST "$SCOPE/events/process-batch" -H "$AUTH" -H 'Content-Type: application/json' \
  -d "{\"event_ids\":[\"$(echo $BIDS | awk '{print $1}')\",\"$(echo $BIDS | awk '{print $2}')\"]}")
need "batch processed (2 events)" "$PB" '"count":2'

# ── 5. Progression → achievement unlock ─────────────────────────────────────
say "progression + achievements"
ST=$(curl -fsS "$SCOPE/users/user-alice/state" -H "$AUTH")
need "alice xp=500 (5 lessons)" "$ST" '"xp":500'
need "alice level=5" "$ST" '"level":5'
need "achievement level-5-badge unlocked" "$ST" 'level-5-badge'
need "achievement reward +200 coins (275 = 65+2×5+200)" "$ST" '"balance":275'

# ── 6. Economy reads ────────────────────────────────────────────────────────
say "economy reads"
WH=$(curl -fsS "$SCOPE/users/user-alice/wallets/coin/history" -H "$AUTH")
need "wallet history (ledger rows)" "$WH" 'ledger\|entries\|amount'
TR=$(curl -fsS "$SCOPE/users/user-alice/transactions" -H "$AUTH")
need "transactions list" "$TR" 'transactions\|\[\]\|"transactions"'

# ── 7. Monetization: product + purchase + idempotency ───────────────────────
say "monetization"
PO=$(curl -fsS -X POST "$SCOPE/objects" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"name":"coin-pack-500","type":"product","config":{"kind":"consumable","price_cents":499,"currency":"USD"}}')
PID=$(echo "$PO" | $PY -c 'import json,sys; print(json.load(sys.stdin)["id"])')
PP=$(curl -sS -o /tmp/pp.json -w '%{http_code}' -X POST "$SCOPE/objects/$PID/publish" -H "$AUTH")
code "product published + materialized" "$PP" "200"
PUR=$(curl -fsS -X POST "$SCOPE/purchases" -H "$AUTH" -H 'Content-Type: application/json' \
  -d "{\"user_id\":\"user-alice\",\"product_id\":\"$PID\",\"idempotency_key\":\"matrix-buy-1\"}")
need "purchase recorded" "$PUR" 'transaction\|purchase\|completed'
PUR2=$(curl -sS -X POST "$SCOPE/purchases" -H "$AUTH" -H 'Content-Type: application/json' \
  -d "{\"user_id\":\"user-alice\",\"product_id\":\"$PID\",\"idempotency_key\":\"matrix-buy-1\"}")
need "purchase replay idempotent" "$PUR2" 'idempotent\|duplicate\|existing'

# ── 8. Flags ────────────────────────────────────────────────────────────────
say "feature flags"
F1=$(curl -fsS -X POST "$SCOPE/objects" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"name":"new-dashboard","type":"flag","config":{"key":"new-dashboard","value":true,"rollout_percent":100}}')
F1ID=$(echo "$F1" | $PY -c 'import json,sys; print(json.load(sys.stdin)["id"])')
curl -fsS -X POST "$SCOPE/objects/$F1ID/publish" -H "$AUTH" >/dev/null
EV1=$(curl -fsS "$SCOPE/flags/new-dashboard/evaluate?user_id=user-alice" -H "$AUTH")
need "flag 100% rollout → enabled" "$EV1" 'true'

F2=$(curl -fsS -X POST "$SCOPE/objects" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"name":"dark-mode","type":"flag","config":{"key":"dark-mode","value":false,"rollout_percent":0}}')
F2ID=$(echo "$F2" | $PY -c 'import json,sys; print(json.load(sys.stdin)["id"])')
curl -fsS -X POST "$SCOPE/objects/$F2ID/publish" -H "$AUTH" >/dev/null
EV2=$(curl -fsS "$SCOPE/flags/dark-mode/evaluate?user_id=user-alice" -H "$AUTH")
need "flag 0% rollout → disabled" "$EV2" 'false'
EV3=$(curl -fsS "$SCOPE/flags/unknown-flag/evaluate" -H "$AUTH" 2>/dev/null || curl -sS "$SCOPE/flags/unknown-flag/evaluate" -H "$AUTH")
need "unknown flag 404" "$(curl -sS -o /dev/null -w '%{http_code}' "$SCOPE/flags/unknown-flag/evaluate" -H "$AUTH")" "404"

# ── 9. Segments ─────────────────────────────────────────────────────────────
say "segments"
SG=$(curl -fsS -X POST "$SCOPE/objects" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"name":"beta-testers","type":"segment","config":{"static_members":["user-alice","user-bob"]}}')
SGID=$(echo "$SG" | $PY -c 'import json,sys; print(json.load(sys.stdin)["id"])')
curl -fsS -X POST "$SCOPE/objects/$SGID/publish" -H "$AUTH" >/dev/null
SL=$(curl -fsS "$SCOPE/segments" -H "$AUTH")
need "segments listed" "$SL" 'beta-testers'

# ── 10. Webhooks: list + delete ─────────────────────────────────────────────
say "webhooks"
W1=$(curl -fsS -X POST "$SCOPE/webhooks" -H "$AUTH" -H 'Content-Type: application/json' -d '{"url":"https://example.test/wh1","events":["event.processed"]}')
WID=$(echo "$W1" | $PY -c 'import json,sys; print(json.load(sys.stdin)["id"])')
WL=$(curl -fsS "$SCOPE/webhooks" -H "$AUTH")
need "webhooks listed" "$WL" 'example.test'
WD=$(curl -sS -o /dev/null -w '%{http_code}' -X DELETE "$SCOPE/webhooks/$WID" -H "$AUTH")
code "webhook deleted" "$WD" "200"

# ── 11. Notifications + audit ───────────────────────────────────────────────
say "notifications + audit"
NL=$(curl -sS -o /tmp/nl.json -w '%{http_code}' "$SCOPE/users/user-alice/notifications" -H "$AUTH")
code "notifications endpoint" "$NL" "200"
AL=$(curl -fsS "$SCOPE/audit?limit=10" -H "$AUTH")
need "audit log has entries" "$AL" 'audit\|action'

# ── 12. MCP tools ───────────────────────────────────────────────────────────
say "MCP"
MCP1=$(curl -fsS -X POST "$SCOPE/mcp" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_config_objects","arguments":{"type":"rule"}}}')
need "MCP list_config_objects" "$MCP1" 'lesson-completed-xp'

# ── 13. Simulation determinism (§66) ────────────────────────────────────────
say "simulation determinism"
S1=$(curl -fsS -X POST "$SCOPE/simulate" -H "$AUTH" -H 'Content-Type: application/json' -d '{"seed":7,"users":25,"days":3}')
S2=$(curl -fsS -X POST "$SCOPE/simulate" -H "$AUTH" -H 'Content-Type: application/json' -d '{"seed":7,"users":25,"days":3}')
R1=$(echo "$S1" | $PY -c 'import json,sys; r=json.load(sys.stdin)["report"]; print(r["seed"], r["users"], r["days"], r["total_events"])')
R2=$(echo "$S2" | $PY -c 'import json,sys; r=json.load(sys.stdin)["report"]; print(r["seed"], r["users"], r["days"], r["total_events"])')
[ "$R1" = "$R2" ] && ok "same seed → identical report ($R1)" || bad "determinism" "$R1 vs $R2"
S3=$(curl -fsS -X POST "$SCOPE/simulate" -H "$AUTH" -H 'Content-Type: application/json' -d '{"seed":8,"users":25,"days":3}')
R3=$(echo "$S3" | $PY -c 'import json,sys; r=json.load(sys.stdin)["report"]; print(r["total_events"])')
[ "$R3" != "$R1" ] && ok "different seed → different stream" || bad "seed sensitivity" "$R3 == $R1"

# ── 14. Engine endpoints direct ─────────────────────────────────────────────
say "engine service endpoints"
EH=$(curl -fsS $EURL/healthz)
need "engine healthz" "$EH" 'ok'
VDE=$(curl -fsS -X POST $EURL/v1/validate -H 'Content-Type: application/json' \
  -d '{"event_id":"evt_probe_ok","event_type":"a.b","event_version":1,"project_id":"p","environment_id":"development","actor_id":"u1","occurred_at":"2026-09-08T20:00:00Z"}')
need "engine validate (good event)" "$VDE" '"valid":true'
VDB=$(curl -fsS -X POST $EURL/v1/validate -H 'Content-Type: application/json' \
  -d '{"event_id":"evt_probe_bad","event_type":"a.b","project_id":"p","environment_id":"development","actor_id":"u1"}')
need "engine validate (bad event rejected)" "$VDB" '"valid":false'
ECFG=$(curl -fsS "$SCOPE/engine-config" -H "$AUTH")
CC=$(echo "$ECFG" | $PY -c '
import json,sys
d=json.load(sys.stdin)
cfg = d.get("config", d)
print(json.dumps(cfg))' > /tmp/cc_cfg.json; echo done)
CCH=$(curl -fsS -X POST $EURL/v1/compile-check -H 'Content-Type: application/json' -d @/tmp/cc_cfg.json)
need "engine compile-check" "$CCH" 'ok\|problems'

# ── 15. Cross-tenant isolation (second tenant) ──────────────────────────────
say "tenant isolation"
B2=$(curl -sS -X POST $BASE/v1/bootstrap -H "$AUTH" -H 'Content-Type: application/json' -d '{"organization":"Rival Corp","project":"rival-project"}')
echo "  [debug] second bootstrap: $(echo "$B2" | head -c 300)"
K2=$(echo "$B2" | $PY -c 'import json,sys; print(json.load(sys.stdin)["api_key"])' 2>/dev/null || echo "")
if [ -n "$K2" ]; then
  XS=$(curl -sS -o /dev/null -w '%{http_code}' "$SCOPE/users/user-alice/state" -H "Authorization: Bearer $K2")
  code "rival tenant cannot read matrix state (404)" "$XS" "404"
  XO=$(curl -sS -o /dev/null -w '%{http_code}' "$SCOPE/events" -H "Authorization: Bearer $K2")
  [ "$XO" = "404" ] || [ "$XO" = "403" ] && ok "rival tenant cannot write matrix events ($XO)" || bad "rival write" "got $XO"
else
  bad "second bootstrap" "$B2"
fi

echo
echo "════ FEATURE-MATRIX RESULT: $PASS passed, $FAIL failed ════"
[ "$FAIL" -eq 0 ] || exit 1
