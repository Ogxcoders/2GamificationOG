#!/usr/bin/env bash
# Probe the engine /v1/process directly with the real compiled config.
set -u
PGWRAP=$HOME/toolchains/pgwrap
PLATFORM=/home/z/my-project/platform
export PATH=$PGWRAP:$PATH

pkill -f 'control-plane' 2>/dev/null; pkill -f 'engine-service' 2>/dev/null; pkill -f 'postgres -D' 2>/dev/null; sleep 1

if ! pg_isready -h 127.0.0.1 -p 5432 >/dev/null 2>&1; then
  postgres -D /tmp/pgdata > /tmp/pg.log 2>&1 &
  for i in $(seq 1 20); do pg_isready -h 127.0.0.1 -p 5432 >/dev/null 2>&1 && break; sleep 0.5; done
fi

ENGINE_BIND=127.0.0.1:8081 RUST_LOG=debug $PLATFORM/target/debug/engine-service > /tmp/engine.log 2>&1 &
for i in $(seq 1 30); do curl -fsS http://127.0.0.1:8081/healthz >/dev/null 2>&1 && break; sleep 0.5; done

# Dump the compiled config to a file
psql -h 127.0.0.1 -U postgres -d platform -tc "SELECT config FROM engine_configs ORDER BY version DESC LIMIT 1;" | python3 -c "import json,sys; print(json.dumps(json.loads(sys.stdin.read().strip()), indent=1))" > /tmp/engine_config.json
echo "config keys: $(python3 -c "import json; d=json.load(open('/tmp/engine_config.json')); print({k: (len(v) if isinstance(v,list) else v) for k,v in d.items()})")"

# One rule entry for inspection
python3 -c "
import json
d=json.load(open('/tmp/engine_config.json'))
print('RULE:', json.dumps(d['rules'][0], indent=1))
print('CHALLENGE:', json.dumps(d['challenges'][0], indent=1))
print('STREAK:', json.dumps(d['streaks'][0], indent=1))
print('CURRENCY:', json.dumps(d['currencies'][0], indent=1))
" 2>&1 | head -60

# Build a process request: event + config + state
python3 - > /tmp/process_req.json << 'PYEOF'
import json
cfg = json.load(open('/tmp/engine_config.json'))
req = {
  "event": {
    "event_id": "evt_probe_1",
    "event_type": "lesson.completed",
    "event_version": 1,
    "project_id": cfg["project_id"],
    "environment_id": cfg["environment_id"],
    "actor_id": "user-alice",
    "subject_id": "",
    "source": "server",
    "occurred_at": "2026-09-08T20:00:00Z",
    "payload": {"lesson_id": "l1", "points": 25},
    "metadata": {}
  },
  "config": cfg,
  "state": {"user": {"user_id": "user-alice", "attributes": {}},
            "variables": {},
            "wallets": {},
            "rule_state": {},
            "progress": {}}
}
print(json.dumps(req))
PYEOF

echo "=== PROCESS RESPONSE ==="
curl -sS -i -X POST http://127.0.0.1:8081/v1/process -H 'Content-Type: application/json' -d @/tmp/process_req.json 2>&1 | head -20
echo
echo "=== ENGINE LOG ==="
tail -20 /tmp/engine.log
