//! Canonical event contract (§10).

use crate::ids::*;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;

/// The universal nervous-system message. Field set is fixed; payloads are
/// schema-validated at the gateway before reaching the engine.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct CanonicalEvent {
    pub event_id: String,
    pub event_type: String,
    #[serde(default)]
    pub event_version: i32,
    pub project_id: String,
    pub environment_id: String,
    #[serde(default)]
    pub actor_id: String,
    #[serde(default)]
    pub subject_id: String,
    #[serde(default)]
    pub source: String,
    /// RFC3339; set by gateway if the client omitted it.
    pub occurred_at: String,
    #[serde(default)]
    pub received_at: String,
    #[serde(default)]
    pub correlation_id: String,
    #[serde(default)]
    pub causation_id: String,
    #[serde(default)]
    pub idempotency_key: String,
    #[serde(default)]
    pub payload: HashMap<String, serde_json::Value>,
    #[serde(default, deserialize_with = "crate::null_map::deserialize")]
    pub metadata: HashMap<String, serde_json::Value>,
}

impl Default for CanonicalEvent {
    fn default() -> Self {
        let now = chrono::Utc::now().to_rfc3339();
        CanonicalEvent {
            event_id: new_uuid(),
            event_type: String::new(),
            event_version: 1,
            project_id: String::new(),
            environment_id: String::new(),
            actor_id: String::new(),
            subject_id: String::new(),
            source: "server".into(),
            occurred_at: now.clone(),
            received_at: now,
            correlation_id: new_uuid(),
            causation_id: String::new(),
            idempotency_key: String::new(),
            payload: HashMap::new(),
            metadata: HashMap::new(),
        }
    }
}

impl CanonicalEvent {
    /// Convenience constructor with the required identifying fields.
    pub fn new(project_id: &str, environment_id: &str, actor_id: &str, event_type: &str) -> Self {
        let mut e = CanonicalEvent::default();
        e.project_id = project_id.into();
        e.environment_id = environment_id.into();
        e.actor_id = actor_id.into();
        e.event_type = event_type.into();
        e
    }

    pub fn with_payload(mut self, k: &str, v: serde_json::Value) -> Self {
        self.payload.insert(k.into(), v);
        self
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn event_deserializes_go_payload_shape() {
        // Go control plane: nil metadata -> null; missing optional fields.
        let raw = r#"{
            "event_id":"e1","event_type":"lesson.completed","event_version":1,
            "project_id":"p1","environment_id":"development","actor_id":"u1",
            "occurred_at":"2026-09-08T10:00:00Z","metadata":null
        }"#;
        let e: CanonicalEvent = serde_json::from_str(raw).unwrap();
        assert!(e.metadata.is_empty());
        assert_eq!(e.event_type, "lesson.completed");
    }
}
