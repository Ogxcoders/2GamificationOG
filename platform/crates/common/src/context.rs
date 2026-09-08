//! Processing context (§12) and path resolution over context values.
//!
//! The context is a flat, dotted-key scope (`user.level`, `event.payload.points`,
//! `metrics.daily_xp`) plus a timezone reference for time-windowed evaluation.

use serde::{Deserialize, Serialize};
use std::collections::HashMap;

/// Scope values used by rule conditions and formula variables.
/// Flat dotted keys keep evaluation O(1) and deterministic.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct Context {
    #[serde(flatten)]
    pub values: HashMap<String, serde_json::Value>,
    /// IANA timezone of the actor (defaults resolved by control plane).
    #[serde(default)]
    pub timezone: String,
    /// Project-level timezone fallback.
    #[serde(default)]
    pub project_timezone: String,
    /// Evaluation instant (RFC3339). Defaults to engine "now" when absent.
    #[serde(default)]
    pub instant: Option<String>,
}

impl Context {
    pub fn new() -> Self {
        Context::default()
    }

    pub fn set(mut self, key: impl Into<String>, value: serde_json::Value) -> Self {
        self.values.insert(key.into(), value);
        self
    }

    /// Exact-key lookup.
    pub fn get(&self, key: &str) -> Option<&serde_json::Value> {
        self.values.get(key)
    }

    /// Effective timezone (actor timezone, else project, else UTC).
    pub fn effective_timezone(&self) -> &str {
        if !self.timezone.is_empty() {
            &self.timezone
        } else if !self.project_timezone.is_empty() {
            &self.project_timezone
        } else {
            "UTC"
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn context_flat_scope() {
        let ctx = Context::new()
            .set("user.level", serde_json::json!(5))
            .set("event.type", serde_json::json!("lesson.completed"));
        assert_eq!(ctx.get("user.level"), Some(&serde_json::json!(5)));
        assert_eq!(ctx.effective_timezone(), "UTC");
    }

    #[test]
    fn flatten_scope_from_go() {
        // Go serializes the scope as a flat object of dotted keys; known
        // struct fields (timezone, instant) are parsed out of the same object.
        let c: Context = serde_json::from_str(
            r#"{"user.level":5,"event.type":"lesson.completed","timezone":"Europe/Berlin"}"#,
        )
        .unwrap();
        assert_eq!(c.get("user.level"), Some(&serde_json::json!(5)));
        assert!(c.get("timezone").is_none());
        assert_eq!(c.timezone, "Europe/Berlin");
        assert!(c.effective_timezone() == "Europe/Berlin");
    }
}
