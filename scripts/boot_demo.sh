#!/usr/bin/env bash
# Boot the FULL stack WITH seeded demo data and serve the admin UI.
# Leaves processes running (parent command keeps the session alive).
set -u
PGWRAP=$HOME/toolchains/pgwrap
PGDATA=/tmp/pgdata
PLATFORM=/home/z/my-project/platform
export PATH=$PGWRAP:$PATH

pkill -f 'control-plane' 2>/dev/null; pkill -f 'engine-service' 2>/dev/null
pkill -f 'postgres -D' 2>/dev/null; pkill -f 'http-server' 2>/dev/null; sleep 1

if ! pg_isready -h 127.0.0.1 -p 5432 >/dev/null 2>&1; then
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

# Serve admin UI (python http.server, CORS-free: the app calls the API cross-origin;
# control-plane has CORS middleware allowing all origins in dev).
cd $PLATFORM/apps/admin/dist && nohup python3 -m http.server 5174 > /tmp/ui.log 2>&1 &
sleep 1

# Seed: tenant + objects + events (reuse the matrix setup, then EXTRA events for charts).
BASE=http://127.0.0.1:8080
BOOT=$(curl -fsS -X POST $BASE/v1/bootstrap -H 'Content-Type: application/json' -d '{"organization":"Demo Org","project":"demo-project"}')
KEY=$(echo "$BOOT" | python3 -c 'import json,sys; print(json.load(sys.stdin)["api_key"])')
PROJ=$(echo "$BOOT" | python3 -c 'import json,sys; print(json.load(sys.stdin)["project"]["id"])')
echo "$KEY" > /tmp/uep-demo-key
echo "PROJECT=$PROJ"

python3 << PYEOF
import json, urllib.request
base, bearer = "$BASE/v1/projects/$PROJ/environments/development", "$KEY"
fx = json.load(open("$PLATFORM/packages/testing/fixtures/golden-path.json"))
for obj in fx["objects"]:
    body = json.dumps({"name": obj["name"], "type": obj["type"], "config": obj["config"]}).encode()
    req = urllib.request.Request(f"{base}/objects", data=body, method="POST")
    req.add_header("Content-Type", "application/json"); req.add_header("Authorization", "Bearer "+bearer)
    oid = json.load(urllib.request.urlopen(req))["id"]
    pub = urllib.request.Request(f"{base}/objects/{oid}/publish", data=b"{}", method="POST")
    pub.add_header("Content-Type", "application/json"); pub.add_header("Authorization", "Bearer "+bearer)
    urllib.request.urlopen(pub).read()
# extra users + events for a livelier demo
extra = []
for u in ["user-dave","user-eve","user-frank","user-grace"]:
    for l in range(2):
        extra.append({"event_type":"lesson.completed","actor_id":u,"payload":{"lesson_id":f"l{u}-{l}"}})
events = fx["events"] + extra
req = urllib.request.Request(f"{base}/events", data=json.dumps({"events": events}).encode(), method="POST")
req.add_header("Content-Type", "application/json"); req.add_header("Authorization", "Bearer "+bearer)
r = json.load(urllib.request.urlopen(req))
for res in r["results"]:
    req = urllib.request.Request(f"{base}/events/{res['event_id']}/process", data=b"{}", method="POST")
    req.add_header("Authorization", "Bearer "+bearer)
    urllib.request.urlopen(req).read()
print("seeded:", len(r["results"]), "events")
PYEOF

echo "STACK READY"
echo "  API:      $BASE  (key: $(cat /tmp/uep-demo-key))"
echo "  UI:       http://127.0.0.1:5174"
