#!/usr/bin/env bash
# Debug: boot stack, capture raw bootstrap response + cp log.
set -u
PGWRAP=$HOME/toolchains/pgwrap
PGDATA=/tmp/pgdata
PLATFORM=/home/z/my-project/platform
export PATH=$PGWRAP:$PATH

pkill -f 'control-plane' 2>/dev/null; pkill -f 'engine-service' 2>/dev/null
pkill -f 'postgres -D' 2>/dev/null; sleep 1

if ! pg_isready -h 127.0.0.1 -p 5432 >/dev/null 2>&1; then
  if [ ! -f $PGDATA/PG_VERSION ]; then
    rm -rf $PGDATA && mkdir -p $PGDATA && chmod 700 $PGDATA
    initdb -D $PGDATA -U postgres --auth=trust -E UTF8 >/dev/null 2>&1
    printf "listen_addresses = '127.0.0.1'\nport = 5432\nunix_socket_directories = '/tmp'\n" >> $PGDATA/postgresql.conf
  fi
  postgres -D $PGDATA > /tmp/pg.log 2>&1 &
  for i in $(seq 1 20); do pg_isready -h 127.0.0.1 -p 5432 >/dev/null 2>&1 && break; sleep 0.5; done
fi
echo "PG: $(pg_isready -h 127.0.0.1 -p 5432 2>&1)"
psql -h 127.0.0.1 -U postgres -c "DROP SCHEMA public CASCADE; CREATE SCHEMA public;" >/dev/null 2>&1
psql -h 127.0.0.1 -U postgres -tc "SELECT 1 FROM pg_database WHERE datname='platform'" | grep -q 1 || psql -h 127.0.0.1 -U postgres -c "CREATE DATABASE platform" >/dev/null

ENGINE_BIND=127.0.0.1:8081 RUST_LOG=warn $PLATFORM/target/debug/engine-service > /tmp/engine.log 2>&1 &
for i in $(seq 1 30); do curl -fsS http://127.0.0.1:8081/healthz >/dev/null 2>&1 && break; sleep 0.5; done
echo "ENGINE: $(curl -fsS http://127.0.0.1:8081/healthz 2>&1)"

DATABASE_URL="postgres://postgres:postgres@127.0.0.1:5432/platform?sslmode=disable" \
ENGINE_URL="http://127.0.0.1:8081" \
CONTROL_PLANE_BIND="127.0.0.1:8080" \
/tmp/control-plane > /tmp/cp.log 2>&1 &
for i in $(seq 1 30); do curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1 && break; sleep 0.5; done
echo "CP: $(curl -fsS http://127.0.0.1:8080/healthz 2>&1)"

echo "=== RAW BOOTSTRAP RESPONSE ==="
curl -sS -i -X POST http://127.0.0.1:8080/v1/bootstrap -H 'Content-Type: application/json' -d '{"organization":"Debug Corp","project":"debug-project"}' 2>&1 | head -30

echo; echo "=== CP LOG (last 30) ==="
tail -30 /tmp/cp.log
echo; echo "=== TABLES ==="
psql -h 127.0.0.1 -U postgres -d platform -c "\dt" 2>&1 | head -40
