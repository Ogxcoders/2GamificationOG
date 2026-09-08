//! # platform-core-engine
//!
//! The deterministic domain brain (§4 universal flow):
//!
//! ```text
//! CanonicalEvent
//!   → build Context (event payload + user state + config snapshots)
//!   → evaluate Rules (conditions, cooldowns, caps)
//!   → expand Actions into transactional Commands
//!   → produce DecisionTrace + EngineOutcome
//! ```
//!
//! The engine is **pure**: no I/O, no wall-clock (uses event.occurred_at / ctx.instant),
//! no database. The Go control plane owns durability; it feeds state in and
//! applies the returned commands inside one DB transaction (§15 atomicity,
//! §11 idempotency, §70 decision trace).

pub mod state;
pub mod commands;
pub mod process;
pub mod challenge;
pub mod streak;
pub mod achievement;
pub mod workflow;

pub use commands::{Command, CommandKind};
pub use process::{process_event, EngineOutcome, ProcessOptions};
pub use state::ActorState;

use platform_common::EngineError;

/// Validate that an event is well-formed before processing (§10 requirements).
pub fn validate_event(e: &platform_common::CanonicalEvent) -> Result<(), EngineError> {
    if e.event_id.is_empty() {
        return Err(EngineError::validation("event_id", "event_id is required"));
    }
    if e.event_type.is_empty() {
        return Err(EngineError::validation("event_type", "event_type is required"));
    }
    if !e.event_type.contains('.') && e.event_type != "*" {
        return Err(EngineError::validation(
            "event_type",
            "event_type should use dotted namespacing (e.g. lesson.completed)",
        ));
    }
    if e.project_id.is_empty() {
        return Err(EngineError::validation("project_id", "project_id is required"));
    }
    if e.environment_id.is_empty() {
        return Err(EngineError::validation("environment_id", "environment_id is required"));
    }
    if e.occurred_at.is_empty() {
        return Err(EngineError::validation("occurred_at", "occurred_at is required"));
    }
    Ok(())
}

/// Parse the evaluation instant (event time preferred).
pub fn evaluation_instant(e: &platform_common::CanonicalEvent, ctx_instant: Option<&str>) -> chrono::DateTime<chrono::Utc> {
    let raw = ctx_instant
        .filter(|s| !s.is_empty())
        .unwrap_or(e.occurred_at.as_str());
    chrono::DateTime::parse_from_rfc3339(raw)
        .map(|d| d.with_timezone(&chrono::Utc))
        .unwrap_or_else(|_| chrono::Utc::now())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn event_validation() {
        let mut e = platform_common::CanonicalEvent::new("p1", "development", "u1", "lesson.completed");
        assert!(validate_event(&e).is_ok());

        e.event_type = "weird".into();
        assert!(validate_event(&e).is_err());

        e.event_type = "lesson.completed".into();
        e.project_id = String::new();
        assert!(validate_event(&e).is_err());
    }

    #[test]
    fn instant_prefers_event_time() {
        let e = platform_common::CanonicalEvent::new("p1", "development", "u1", "lesson.completed");
        let t = evaluation_instant(&e, Some("2026-09-08T12:00:00Z"));
        assert_eq!(t.to_rfc3339(), "2026-09-08T12:00:00+00:00");
    }
}
