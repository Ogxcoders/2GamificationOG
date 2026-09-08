# Universal Engagement Platform

A production-grade, multi-tenant **gamification + monetization platform**: rules, challenges,
achievements, streaks, progression, economy, leaderboards, seasons, experiments, paywalls,
entitlements — driven by a **deterministic core engine** with a fully auditable decision trace.

Built to the specification in `UNIVERSAL_GAMIFICATION_PLATFORM_MASTER_PLAN-3.md`
(414 sections): every event produces the same outcome whether processed live, replayed,
or simulated.

---

## Architecture

Two services over PostgreSQL (+ Redis for cache/queue):

```
 Client SDKs ──► Control Plane (Go, :8080)  ──► Core Engine (Rust, :8081)
                    │  REST API, tenancy,          │  deterministic evaluation:
                    │  event ingest, validation,    │  rules / formulas / progression /
                    │  persistence, workers,        │  ranking / economy / simulation
                    │  webhooks, MCP endpoint       │
                    ▼                               ▼
               PostgreSQL 16  ◄─────  append-only ledger, outbox, traces
```

**Determinism guarantees**

- Rule/formula evaluation is pure: same context + config ⇒ same commands.
- Economy ledger is append-only with two-pass NET balance validation — no double-spend.
- Ranking is deterministic with explicit tie-breaks (score, achieved_at, user_id).
- Every decision returns a full trace (rules matched, formulas evaluated, commands issued).
- Event processing is idempotent (atomic `ON CONFLICT` dedup).
- Multi-tenant isolation enforced on every query (tenant-scoped invariants A–L).

## Monorepo layout

```
platform/
├── crates/            Rust workspace (9 crates)
│   ├── common/        typed IDs, canonical event, context, time windows, error taxonomy
│   ├── formulas/      safe expression engine (lexer/parser/eval, depth-guarded)
│   ├── rules/         condition + operator evaluation (§13 operator set)
│   ├── progression/   levels, XP curves, track advancement
│   ├── ranking/       deterministic leaderboards with tie handling
│   ├── economy/       ledger, balances, NET validation
│   ├── simulation/    deterministic what-if simulation
│   ├── core-engine/   event → context → rules → commands → trace pipeline
│   └── engine-service/ axum HTTP wrapper (/v1/process /v1/validate /v1/config /v1/simulate)
├── services/
│   └── control-plane/ Go service (20 domain packages)
│       ├── internal/  tenancy, identity, eventing, processing pipeline, configsvc,
│       │              player state, economy, leaderboards, challenges, achievements,
│       │              streaks, progression, inventory, seasons, segments, experiments,
│       │              flags, monetization, subscriptions, entitlements, notifications,
│       │              webhooks (HMAC), analytics, risk, audit, plugins, packs, MCP,
│       │              workflow, workers, engine client
│       ├── internal/db/migrations/  4 migrations, 60+ tables
│       └── cmd/control-plane/       entrypoint with graceful shutdown
├── packages/          canonical JSON schemas + OpenAPI 3.1 + UI schema + fixtures
├── sdk/               6 client SDKs: TypeScript, Kotlin (Android), Swift (iOS),
│                      C# (Unity), C++ (Unreal), GDScript (Godot)
├── apps/admin/        React + Vite admin console (with built dist/)
├── plugins/           example plugin (skill-tree, manifest + TS)
├── packs/             example content packs (daily-streak, productivity)
├── examples/          golden-path example
├── infrastructure/    docker-compose stack + Dockerfiles
├── scripts/           bootstrap.sh (first tenant) + e2e_golden_path.sh
└── Makefile           build / test / run / compose / bootstrap / e2e
```

## Quick start

### Docker (full stack)

```bash
cd platform
docker compose -f infrastructure/docker/docker-compose.yml up -d --build
# postgres + redis + engine(:8081) + control-plane(:8080) + admin
./scripts/bootstrap.sh        # creates the first tenant + admin credentials
./scripts/e2e_golden_path.sh  # end-to-end verification
```

### Local build

```bash
# Rust toolchain + Go 1.23+
cd platform
make build      # cargo build --release + go build + vite build (admin)
make test       # 93 Rust unit tests + go vet
make run        # run engine + control-plane against local postgres
```

## Core concepts

| Concept | Where |
|---|---|
| CanonicalEvent (§10) | `crates/common/src/event.rs` |
| Context assembly (§12) | `crates/common/src/context.rs` |
| Conditions & operators (§13) | `crates/rules/` |
| Safe formulas (§14) | `crates/formulas/` |
| Time windows: rolling/fixed/calendar, DST-safe (§18) | `crates/common/src/time.rs` |
| Append-only ledger + NET validation (§27) | `crates/economy/` |
| Deterministic ranking + ties (§29) | `crates/ranking/` |
| Decision trace (§22) | `crates/common/src/trace.rs` |
| Config lifecycle: draft → published → active | `services/control-plane/internal/configsvc` |
| Processing pipeline (lock → engine → apply → outbox) | `services/control-plane/internal/processing` |
| API keys (`keyID.secret` format, hashed) | `services/control-plane/internal/identity` |
| Webhook delivery (HMAC signatures, retries) | `services/control-plane/internal/webhooks` |
| MCP endpoint (tools/list + tools/call) | `services/control-plane/internal/mcp` |

## Engine HTTP API (Rust service)

| Endpoint | Purpose |
|---|---|
| `POST /v1/process` | event → commands + trace |
| `POST /v1/validate` | config validation without persistence |
| `POST /v1/compile-check` | compile rule/formula sets |
| `GET  /v1/config` | engine config snapshot |
| `POST /v1/simulate` | deterministic what-if simulation |

## Verification status

- `cargo test`: **93/93 pass** across all 9 crates
- `go build ./...` + `go vet`: clean (20 packages, ~5,500 lines)
- `make e2e` / `scripts/e2e_golden_path.sh`: Customer-Zero golden path
  (tenant bootstrap → admin/API key → publish rule/challenge/streak/achievement →
  ingest events → wallet + progress + leaderboard + trace assertions)

## Notes

- The engine is intentionally stateless between calls; the control plane owns all
  persistence and passes compiled config snapshots + player state per event.
- All timestamps are UTC at rest; window arithmetic is DST-safe.
- `.env` is ignored; configuration comes from environment variables
  (`DATABASE_URL`, `ENGINE_URL`, `ENGINE_BIND`, `RUST_LOG`).
