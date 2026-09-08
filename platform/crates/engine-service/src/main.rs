//! engine-service: HTTP wrapper around the pure core engine.
//!
//! Endpoints (control-plane facing):
//! - GET  /healthz, /readyz
//! - POST /v1/process            — process one event (event + config + actor state in)
//! - POST /v1/validate           — validate an event's shape
//! - POST /v1/compile-check      — validate an EngineConfig
//! - POST /v1/simulate           — deterministic cohort simulation

use axum::{extract::State, http::StatusCode, routing::{get, post}, Json, Router};
use platform_common::config::EngineConfig;
use platform_core_engine::state::ActorState;
use platform_core_engine::{process_event, ProcessOptions, EngineOutcome};
use platform_simulation::{generate_events, SimulationRequest, SimulationReport};
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::sync::Arc;
use tower_http::cors::CorsLayer;
use tower_http::trace::TraceLayer;

#[derive(Default)]
struct AppState {
    /// Last-seen config per (project, env) — the control plane pushes configs
    /// before processing; /process also accepts inline config.
    configs: std::sync::Mutex<HashMap<String, EngineConfig>>,
}

impl AppState {
    fn key(project: &str, env: &str) -> String {
        format!("{}:{}", project, env)
    }
}

#[derive(Deserialize)]
struct ProcessRequest {
    event: platform_common::CanonicalEvent,
    #[serde(default)]
    config: Option<EngineConfig>,
    #[serde(default)]
    actor_state: Option<ActorState>,
    #[serde(default)]
    dry_run: bool,
}

#[derive(Serialize)]
struct ProcessResponse {
    ok: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    error: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    error_code: Option<&'static str>,
    #[serde(skip_serializing_if = "Option::is_none")]
    outcome: Option<EngineOutcome>,
}

#[derive(Deserialize)]
struct PushConfigRequest {
    project_id: String,
    environment_id: String,
    config: EngineConfig,
}

async fn healthz() -> Json<serde_json::Value> {
    Json(serde_json::json!({"status": "ok", "service": "engine-service"}))
}

async fn readyz() -> Json<serde_json::Value> {
    Json(serde_json::json!({"ready": true}))
}

async fn process_handler(
    State(state): State<Arc<AppState>>,
    Json(req): Json<ProcessRequest>,
) -> (StatusCode, Json<ProcessResponse>) {
    let mut actor = req.actor_state.unwrap_or_else(|| ActorState::new(&req.event.actor_id));
    let options = ProcessOptions { dry_run: req.dry_run };

    // Config precedence: inline > pushed.
    let empty = EngineConfig::default();
    let cfg = match &req.config {
        Some(c) => c.clone(),
        None => state
            .configs
            .lock()
            .unwrap()
            .get(&AppState::key(&req.event.project_id, &req.event.environment_id))
            .cloned()
            .unwrap_or(empty),
    };

    match process_event(&req.event, &cfg, &mut actor, &options) {
        Ok(outcome) => {
            // Cache the config for subsequent calls without inline config.
            if let Some(c) = req.config {
                state
                    .configs
                    .lock()
                    .unwrap()
                    .insert(AppState::key(&req.event.project_id, &req.event.environment_id), c);
            }
            (
                StatusCode::OK,
                Json(ProcessResponse { ok: true, error: None, error_code: None, outcome: Some(outcome) }),
            )
        }
        Err(e) => (
            StatusCode::UNPROCESSABLE_ENTITY,
            Json(ProcessResponse {
                ok: false,
                error: Some(e.to_string()),
                error_code: Some(e.code()),
                outcome: None,
            }),
        )
    }
}

async fn validate_handler(
    Json(event): Json<platform_common::CanonicalEvent>,
) -> Json<serde_json::Value> {
    match platform_core_engine::validate_event(&event) {
        Ok(()) => Json(serde_json::json!({"valid": true})),
        Err(e) => Json(serde_json::json!({"valid": false, "error": e.to_string(), "code": e.code()})),
    }
}

async fn compile_check_handler(
    Json(config): Json<EngineConfig>,
) -> Json<serde_json::Value> {
    // Structural sanity: duplicates, empty ids, unknown action targets.
    let mut problems: Vec<String> = Vec::new();
    let mut rule_ids: Vec<&str> = config.rules.iter().map(|r| r.id.as_str()).collect();
    rule_ids.sort();
    for w in rule_ids.windows(2) {
        if w[0] == w[1] && !w[0].is_empty() {
            problems.push(format!("duplicate rule id `{}`", w[0]));
        }
    }
    for r in &config.rules {
        if r.id.is_empty() {
            problems.push("rule with empty id".into());
        }
        if r.actions.is_empty() {
            problems.push(format!("rule `{}` has no actions", r.id));
        }
        for a in &r.actions {
            if let platform_common::config::ActionDef::AddCurrency { currency, .. } = a {
                if !config.currencies.iter().any(|c| c.id == *currency) {
                    problems.push(format!(
                        "rule `{}` awards unknown currency `{}` — add the currency or fix the action",
                        r.id, currency
                    ));
                }
            }
        }
    }
    Json(serde_json::json!({
        "valid": problems.is_empty(),
        "problems": problems,
        "counts": {
            "rules": config.rules.len(),
            "challenges": config.challenges.len(),
            "streaks": config.streaks.len(),
            "achievements": config.achievements.len(),
            "currencies": config.currencies.len(),
            "leaderboards": config.leaderboards.len(),
            "workflows": config.workflows.len(),
        }
    }))
}

async fn push_config_handler(
    State(state): State<Arc<AppState>>,
    Json(req): Json<PushConfigRequest>,
) -> Json<serde_json::Value> {
    let v = req.config.version;
    state
        .configs
        .lock()
        .unwrap()
        .insert(AppState::key(&req.project_id, &req.environment_id), req.config);
    Json(serde_json::json!({"ok": true, "version": v}))
}

async fn simulate_handler(
    Json(req): Json<SimulationRequest>,
) -> Json<serde_json::Value> {
    let start = std::time::Instant::now();
    let events = generate_events(&req);
    let total = events.len();
    let report = SimulationReport {
        seed: req.seed,
        users: req.users,
        days: req.days,
        total_events: total,
        total_actions: total, // each event produces >= 1 engine evaluation
        level_distribution: Default::default(),
        currency_totals: Default::default(),
        compute_micros: start.elapsed().as_micros(),
    };
    Json(serde_json::json!({"report": report, "sample": events.iter().take(5).collect::<Vec<_>>()}))
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| "engine_service=info,tower_http=info".into()),
        )
        .init();

    let state = Arc::new(AppState::default());

    let app = Router::new()
        .route("/healthz", get(healthz))
        .route("/readyz", get(readyz))
        .route("/v1/process", post(process_handler))
        .route("/v1/validate", post(validate_handler))
        .route("/v1/compile-check", post(compile_check_handler))
        .route("/v1/config", post(push_config_handler))
        .route("/v1/simulate", post(simulate_handler))
        .layer(TraceLayer::new_for_http())
        .layer(CorsLayer::permissive())
        .with_state(state);

    let addr = std::env::var("ENGINE_BIND")
        .unwrap_or_else(|_| "0.0.0.0:8081".into());
    let listener = tokio::net::TcpListener::bind(&addr).await?;
    tracing::info!("engine-service listening on {}", addr);
    axum::serve(listener, app).await?;
    Ok(())
}
