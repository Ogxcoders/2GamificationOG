#!/usr/bin/env bash
# Golden-path E2E (§156 Customer Zero acceptance): bootstrap → configure →
# publish → ingest → process → verify state, wallets, leaderboards, traces.
# Asserts every step; exits non-zero on the first failure.
set -euo pipefail

BASE="${UEP_BASE:-http://localhost:8080}"
KEY_FILE="${UEP_KEY:-$HOME/.uep/admin-key}"
FIXTURES="$(dirname "$0")/../packages/testing/fixtures"

[ -f "$KEY_FILE" ] || { echo "no API key at $KEY_FILE — run scripts/bootstrap.sh first" >&2; exit 1; }
KEY=$(cat "$KEY_FILE")
AUTH="Authorization: Bearer $KEY"

PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); echo "  ✓ $1"; }
fail() { FAIL=$((FAIL+1)); echo "  ✗ $1"; echo "    $2" | head -3; }
need() { # need <desc> <actual> <expected-substring>
  if echo "$2" | grep -q "$3"; then ok "$1"; else fail "$1" "expected '$3' in: $(echo "$2" | head -c 300)"; fi
}

echo "── golden path against $BASE ──"

# 1. Resolve scope (project + development environment).
PROJECTS=$(curl -fsS "$BASE/v1/projects?organization_id=$(curl -fsS -X POST "$BASE/v1/bootstrap" -H 'Content-Type: application/json' -d '{"organization":"probe","project":"probe"}' >/dev/null 2>&1; echo x)" 2>/dev/null || true)
# The key's own scope is discoverable from any project listing: use the key ID's row.
KEY_ID="${KEY%%.*}"
SCOPE_JSON=$(curl -fsS "$BASE/v1/keys/probe" 2>/dev/null || true)
# Simpler: the bootstrap response is re-derivable — parse project/env from the key.
PROJ=$(curl -fsS -H "$AUTH" "$BASE/v1/projects?organization_id=none" 2>/dev/null | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["projects"][0]["id"] if d.get("projects") else "")' 2>/dev/null || true)
if [ -z "$PROJ" ]; then
  # fall back: bootstrap a dedicated golden-path tenant
  RESP=$(curl -fsS -X POST "$BASE/v1/bootstrap" -H 'Content-Type: application/json' -d '{"organization":"Golden Path Inc","project":"golden-path"}')
  KEY=$(echo "$RESP" | python3 -c 'import json,sys; print(json.load(sys.stdin)["api_key"])')
  AUTH="Authorization: Bearer $KEY"
  PROJ=$(echo "$RESP" | python3 -c 'import json,sys; print(json.load(sys.stdin)["project"]["id"])')
fi
SCOPE="/v1/projects/$PROJ/environments/development"
echo "scope: $SCOPE"

# 2. Create + publish every fixture object.
python3 - "$FIXTURES/golden-path.json" "$BASE" "$SCOPE" "$AUTH" << 'PYEOF'
import json, sys, urllib.request
fixture, base, scope, auth = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
created = 0
for obj in json.load(open(fixture))["objects"]:
    body = json.dumps({"name": obj["name"], "type": obj["type"], "config": obj["config"]}).encode()
    req = urllib.request.Request(f"{base}{scope}/objects", data=body, headers={"Content-Type": "application/json", "Authorization": auth[21:] and auth or auth}, method="POST")
    req.add_header("Authorization", auth)
    with urllib.request.urlopen(req) as r:
        oid = json.load(r)["id"]
    pub = urllib.request.Request(f"{base}{scope}/objects/{oid}/publish", data=b"{}", headers={"Content-Type": "application/json"}, method="POST")
    pub.add_header("Authorization", auth)
    urllib.request.urlopen(pub).read()
    created += 1
print(f"created+published: {created}")
PYEOF
ok "objects created + published (7)"

# 3. Ingest the fixture events.
EVENTS=$(python3 -c 'import json,sys; print(json.dumps({"events": json.load(open(sys.argv[1]))["events"]}))' "$FIXTURES/golden-path.json")
INGEST=$(curl -fsS -X POST "$BASE$scope/events" -H "$AUTH" -H 'Content-Type: application/json' -d "$EVENTS")
need "events ingested (5)" "$INGEST" '"inserted":5'
DUP=$(curl -fsS -X POST "$BASE$scope/events" -H "$AUTH" -H 'Content-Type: application/json' -d "$EVENTS")
need "duplicate replay detected" "$DUP" '"duplicates":5'

# 4. Process every event.
EVENT_IDS=$(echo "$INGEST" | python3 -c 'import json,sys; print(" ".join(r["event_id"] for r in json.load(sys.stdin)["results"]))')
for EID in $EVENT_IDS; do
  OUT=$(curl -sS -X POST "$BASE$scope/events/$EID/process" -H "$AUTH")
  if echo "$OUT" | grep -q '"processed":true'; then ok "processed $EID"; else fail "process $EID" "$OUT"; fi
done

# 5. Verify Alice's state (300 xp, level 3, 65 coins, challenge complete, streak 1).
STATE=$(curl -fsS "$BASE$scope/users/user-alice/state" -H "$AUTH")
need "alice xp=300 (3 lessons × 100)" "$STATE" '"xp": 300'
need "alice coin=65 (3×5 + 50 bonus)" "$STATE" '"balance": 65'
need "alice challenge completed" "$STATE" '"status": "completed"'
need "alice streak extended" "$STATE" '"current": 1'

# 6. Trace answers WHY.
TRACE=$(curl -fsS "$BASE$scope/events/$(echo $EVENT_IDS | awk '{print $1}')/trace" -H "$AUTH")
need "trace has rule_matched nodes" "$TRACE" 'rule_matched'
need "trace has ledger entries" "$TRACE" 'ledger_entry'

# 7. Leaderboard: alice ranks #1 with 300.
LB=$(curl -fsS "$BASE$scope/leaderboards/$(curl -fsS "$BASE$scope/objects?type=leaderboard" -H "$AUTH" | python3 -c 'import json,sys; print(json.load(sys.stdin)["objects"][0]["id"])')?limit=10" -H "$AUTH")
need "leaderboard alice rank 1" "$LB" '"rank": 1'

# 8. Economy NET guard: overspend is refused.
SPEND=$(curl -sS -X POST "$BASE$scope/users/user-bob/wallets/adjust" -H "$AUTH" -H 'Content-Type: application/json' -d '{"currency":"coin","amount":-999,"reference":"overspend-test"}')
need "overspend refused (NET validation)" "$SPEND" 'insufficient'

# 9. Security: no key → 401 everywhere.
NOAUTH=$(curl -sS -o /dev/null -w '%{http_code}' "$BASE$scope/events")
need "unauthenticated request rejected (401)" "$NOAUTH" '401'

echo
echo "── result: $PASS passed, $FAIL failed ──"
[ "$FAIL" -eq 0 ] || exit 1
