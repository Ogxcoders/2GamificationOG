#!/usr/bin/env bash
# Bootstrap the first tenant (Customer Zero step 1, §156) and print the key.
# Requires the stack to be running (make compose or make run).
set -euo pipefail

BASE="${UEP_BASE:-http://localhost:8080}"
ORG="${1:-Acme Learning}"
PROJECT="${2:-Duolingo for Math}"

if ! curl -fsS "$BASE/healthz" >/dev/null 2>&1; then
  echo "control-plane not reachable at $BASE — start the stack first (make compose or make run)" >&2
  exit 1
fi

echo "→ bootstrapping '$ORG / $PROJECT' at $BASE ..."
RESP=$(curl -fsS -X POST "$BASE/v1/bootstrap" \
  -H 'Content-Type: application/json' \
  -d "{\"organization\": \"$ORG\", \"project\": \"$PROJECT\"}")

KEY=$(echo "$RESP" | python3 -c 'import json,sys; print(json.load(sys.stdin)["api_key"])')
echo "$RESP" | python3 -c 'import json,sys; d=json.load(sys.stdin); print("project:", d["project"]["id"]); print("environments:", ", ".join(e["kind"] for e in d["environments"]))'

# Save the key locally (development convenience ONLY).
mkdir -p ~/.uep
echo "$KEY" > ~/.uep/admin-key
chmod 600 ~/.uep/admin-key
echo
echo "API key saved to ~/.uep/admin-key (development convenience — rotate for real deployments)"
echo "Next: ./scripts/e2e_golden_path.sh   # runs the full Customer Zero golden path"
