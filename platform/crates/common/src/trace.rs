//! Decision Trace primitives (§70, §229, §230).
//!
//! Every automated decision produces a trace: a DAG of typed nodes explaining
//! WHY the decision happened — sufficient to answer "why did this user get
//! this reward?" without database spelunking.

use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct TraceNode {
    /// Unique within the trace (n1, n2, ...).
    pub id: String,
    /// Parent node id (roots have empty parent).
    #[serde(default)]
    pub parent: String,
    pub kind: TraceNodeKind,
    /// Human-readable summary of the node.
    #[serde(default)]
    pub label: String,
    /// Structured detail: inputs, computed values, matched ids.
    #[serde(default)]
    pub detail: serde_json::Value,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum TraceNodeKind {
    EventIngress,
    ContextResolved,
    ConditionEvaluated,
    FormulaEvaluated,
    RuleMatched,
    RuleSkipped,
    ActionExecuted,
    ActionFailed,
    CommandApplied,
    CommandRejected,
    ChallengeProgressed,
    ChallengeCompleted,
    AchievementUnlocked,
    StreakUpdated,
    LedgerEntry,
    LevelUp,
    LeaderboardUpdated,
    EntitlementGranted,
    WorkflowStep,
    NotificationQueued,
}

/// Full trace produced by one event-processing run.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct DecisionTrace {
    pub trace_id: String,
    pub event_id: String,
    pub correlation_id: String,
    #[serde(default)]
    pub project_id: String,
    #[serde(default)]
    pub environment_id: String,
    #[serde(default)]
    pub actor_id: String,
    #[serde(default)]
    pub config_version: i64,
    #[serde(default, deserialize_with = "crate::null_vec::deserialize")]
    pub nodes: Vec<TraceNode>,
}

impl DecisionTrace {
    pub fn new(event_id: &str, correlation_id: &str, project: &str, env: &str, actor: &str) -> Self {
        DecisionTrace {
            trace_id: crate::new_uuid(),
            event_id: event_id.into(),
            correlation_id: correlation_id.into(),
            project_id: project.into(),
            environment_id: env.into(),
            actor_id: actor.into(),
            ..Default::default()
        }
    }

    /// Append a node with an auto id; returns the node id for parenting.
    pub fn push(&mut self, parent: &str, kind: TraceNodeKind, label: impl Into<String>, detail: serde_json::Value) -> String {
        let id = format!("n{}", self.nodes.len() + 1);
        self.nodes.push(TraceNode {
            id: id.clone(),
            parent: parent.into(),
            kind,
            label: label.into(),
            detail,
        });
        id
    }

    pub fn root(&self) -> &str {
        self.nodes.first().map(|n| n.id.as_str()).unwrap_or("")
    }

    /// Compact "why" summary for API responses.
    pub fn summary(&self) -> Vec<String> {
        self.nodes
            .iter()
            .filter(|n| matches!(n.kind, TraceNodeKind::RuleMatched | TraceNodeKind::ActionExecuted | TraceNodeKind::ActionFailed | TraceNodeKind::CommandRejected))
            .map(|n| n.label.clone())
            .collect()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn trace_builds_dag() {
        let mut t = DecisionTrace::new("e1", "c1", "p1", "development", "u1");
        let root = t.push("", TraceNodeKind::EventIngress, "event lesson.completed", serde_json::json!({}));
        let rule = t.push(&root, TraceNodeKind::RuleMatched, "rule r1 matched", serde_json::json!({"rule":"r1"}));
        t.push(&rule, TraceNodeKind::ActionExecuted, "award 100 xp", serde_json::json!({"xp":100}));
        assert_eq!(t.nodes.len(), 3);
        assert_eq!(t.nodes[2].parent, rule);
        assert_eq!(t.summary().len(), 2);
    }
}
