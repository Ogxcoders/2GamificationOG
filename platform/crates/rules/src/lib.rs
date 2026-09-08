//! # platform-rules
//!
//! Rule evaluation (§13): the full operator set over the processing context,
//! plus rule-level features — priority ordering, cooldown, frequency caps,
//! status gating, and per-rule evaluation traces.
//!
//! Operand semantics (previous-session lesson): a `Path` operand that cannot
//! resolve AND has no dot is a bare literal; dotted paths that fail to resolve
//! evaluate to Null.

pub mod evaluate;

pub use evaluate::{evaluate_condition, rule_eligible, RuleEvaluation};

use platform_common::{Condition, ObjectStatus, RuleDef};
use serde::{Deserialize, Serialize};
use std::collections::HashMap;

/// Mutable per-actor rule runtime state (cooldowns / frequency counters).
/// Kept by the engine in-memory and by the control plane in Postgres.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct RuleState {
    /// rule_id -> last fired epoch seconds.
    #[serde(default)]
    pub last_fired: HashMap<String, i64>,
    /// rule_id -> (window_key -> count).
    #[serde(default)]
    pub fire_counts: HashMap<String, HashMap<String, i64>>,
}

impl RuleState {
    /// Cooldown check — absent metric means "never fired" (NOT epoch 0).
    pub fn cooldown_active(&self, rule: &RuleDef, now_epoch: i64) -> bool {
        if rule.cooldown_seconds <= 0 {
            return false;
        }
        match self.last_fired.get(&rule.id) {
            Some(last) => now_epoch.saturating_sub(*last) < rule.cooldown_seconds,
            None => false,
        }
    }

    /// Frequency cap check against the resolved window key.
    pub fn frequency_capped(&self, rule: &RuleDef, window_key: &str) -> bool {
        if rule.frequency_cap <= 0 {
            return false;
        }
        let used = self
            .fire_counts
            .get(&rule.id)
            .and_then(|m| m.get(window_key))
            .copied()
            .unwrap_or(0);
        used >= rule.frequency_cap
    }

    pub fn record_fire(&mut self, rule_id: &str, now_epoch: i64, window_key: &str) {
        self.last_fired.insert(rule_id.into(), now_epoch);
        self.fire_counts
            .entry(rule_id.into())
            .or_default()
            .entry(window_key.into())
            .and_modify(|c| *c += 1)
            .or_insert(1);
    }
}

/// Is a rule eligible by status (runtime view) and event type?
pub fn rule_matches_event(rule: &RuleDef, event_type: &str) -> bool {
    let active = matches!(rule.status, ObjectStatus::Active);
    if !active {
        return false;
    }
    rule.event_type == "*" || rule.event_type == event_type
}

#[cfg(test)]
mod tests {
    use super::*;
    use platform_common::config::ActionDef;
    use platform_common::{Condition, Operand};

    fn rule(id: &str, cooldown: i64, cap: i64) -> RuleDef {
        RuleDef {
            id: id.into(),
            name: id.into(),
            priority: 0,
            event_type: "lesson.completed".into(),
            condition: Condition::Eq {
                field: Operand::path("event.type"),
                value: Operand::value(serde_json::json!("lesson.completed")),
            },
            actions: vec![ActionDef::AwardXp { track: "default".into(), amount: 100 }],
            cooldown_seconds: cooldown,
            frequency_cap: cap,
            frequency_window: None,
            status: ObjectStatus::Active,
        }
    }

    #[test]
    fn absent_cooldown_is_never_active() {
        let r = rule("r1", 60, 0);
        let mut st = RuleState::default();
        // No record at all — must NOT count as fired at epoch 0.
        assert!(!st.cooldown_active(&r, 1_000_000));
    }

    #[test]
    fn cooldown_blocks_within_window() {
        let r = rule("r1", 60, 0);
        let mut st = RuleState::default();
        st.record_fire("r1", 1_000_000, "day/2026-09-08");
        assert!(st.cooldown_active(&r, 1_000_030));
        assert!(!st.cooldown_active(&r, 1_000_061));
    }

    #[test]
    fn frequency_cap_counts_per_window() {
        let r = rule("r1", 0, 3);
        let mut st = RuleState::default();
        for _ in 0..3 {
            st.record_fire("r1", 1_000_000, "day/2026-09-08");
        }
        assert!(st.frequency_capped(&r, "day/2026-09-08"));
        assert!(!st.frequency_capped(&r, "day/2026-09-09"));
    }

    #[test]
    fn status_and_event_gating() {
        let r = rule("r1", 0, 0);
        assert!(rule_matches_event(&r, "lesson.completed"));
        assert!(!rule_matches_event(&r, "other.event"));
        let mut paused = r.clone();
        paused.status = ObjectStatus::Paused;
        assert!(!rule_matches_event(&paused, "lesson.completed"));
        let mut wildcard = r.clone();
        wildcard.event_type = "*".into();
        assert!(rule_matches_event(&wildcard, "anything"));
    }
}
