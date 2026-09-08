//! Engine-facing configuration contract (§6/§7 + §13-§31 domain specs).
//!
//! This is the *canonical* (compiled) format the Go control plane materializes
//! from authored objects and pushes to the engine — type-tagged, versioned,
//! immutable snapshots (§127).

use crate::ObjectStatus;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;

use crate::time::TimeWindow;

// ─────────────────────────────────────────────────────────────────────────────
// Rules
// ─────────────────────────────────────────────────────────────────────────────

/// Rule definition in canonical form.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct RuleDef {
    pub id: String,
    pub name: String,
    #[serde(default)]
    pub priority: i32,
    /// Event type this rule observes ("*" matches all).
    pub event_type: String,
    pub condition: crate::Condition,
    pub actions: Vec<ActionDef>,
    /// Cooldown seconds per actor; 0 = none.
    #[serde(default)]
    pub cooldown_seconds: i64,
    /// Max fires per actor per window; 0 = uncapped.
    #[serde(default)]
    pub frequency_cap: i64,
    #[serde(default)]
    pub frequency_window: Option<TimeWindow>,
    #[serde(default)]
    pub status: ObjectStatus,
}

// ─────────────────────────────────────────────────────────────────────────────
// Actions (§15)
// ─────────────────────────────────────────────────────────────────────────────

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum ActionDef {
    AwardXp {
        track: String,
        amount: i64,
    },
    AddCurrency {
        currency: String,
        amount: i64,
    },
    SpendCurrency {
        currency: String,
        amount: i64,
    },
    GrantItem {
        item: String,
        quantity: i64,
    },
    GrantReward {
        reward: String,
    },
    UpdateProgress {
        challenge: String,
        amount: i64,
    },
    StartChallenge {
        challenge: String,
    },
    CompleteChallenge {
        challenge: String,
    },
    UnlockAchievement {
        achievement: String,
    },
    UpdateStreak {
        streak: String,
    },
    UpdateLeaderboard {
        leaderboard: String,
        /// Formula or literal score delta.
        score: String,
    },
    GrantEntitlement {
        entitlement: String,
        #[serde(default)]
        duration_seconds: i64,
    },
    Notify {
        template: String,
        #[serde(default, deserialize_with = "crate::null_map::deserialize")]
        params: HashMap<String, String>,
    },
    ShowPaywall {
        paywall: String,
    },
    StartWorkflow {
        workflow: String,
    },
    SetVariable {
        key: String,
        value: serde_json::Value,
    },
    EmitEvent {
        event_type: String,
        #[serde(default, deserialize_with = "crate::null_map::deserialize")]
        payload: HashMap<String, serde_json::Value>,
    },
    CallWebhook {
        endpoint: String,
    },
}

// ─────────────────────────────────────────────────────────────────────────────
// Challenges (§21)
// ─────────────────────────────────────────────────────────────────────────────

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ChallengeSpec {
    pub id: String,
    pub name: String,
    /// Event that starts the challenge (empty = auto-start on availability).
    #[serde(default)]
    pub trigger_event: String,
    /// Event type that progresses the challenge (§21 progress source).
    #[serde(default)]
    pub progress_event_type: String,
    /// Target amount to complete.
    #[serde(default)]
    pub target: i64,
    /// Cadence window (daily/weekly/monthly/custom).
    #[serde(default)]
    pub window: Option<TimeWindow>,
    /// Rewards issued on completion.
    #[serde(default, deserialize_with = "crate::null_vec::deserialize")]
    pub rewards: Vec<ChallengeReward>,
    #[serde(default)]
    pub repeatability: RepeatMode,
    #[serde(default)]
    pub status: ObjectStatus,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, Default)]
#[serde(rename_all = "lowercase")]
pub enum RepeatMode {
    #[default]
    Once,
    Daily,
    Weekly,
    Monthly,
    Unlimited,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ChallengeReward {
    #[serde(rename = "type")]
    pub kind: RewardKind,
    #[serde(default)]
    pub amount: i64,
    /// XP track, currency id, item id or entitlement key depending on kind.
    #[serde(default)]
    pub target: String,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum RewardKind {
    Xp,
    Currency,
    Item,
    Entitlement,
}

// ─────────────────────────────────────────────────────────────────────────────
// Streaks (§24)
// ─────────────────────────────────────────────────────────────────────────────

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct StreakSpec {
    pub id: String,
    pub name: String,
    pub event_type: String,
    /// Cadence window — uses the Time Engine (never hardcoded daily).
    pub window: TimeWindow,
    /// Grace period in seconds after a missed window.
    #[serde(default)]
    pub grace_seconds: i64,
    /// Freeze allowances per period.
    #[serde(default)]
    pub freezes_allowed: i32,
    /// Reward multiplier by streak length (e.g. 7 -> 2.0).
    #[serde(default)]
    pub multiplier: Option<String>,
    #[serde(default)]
    pub status: ObjectStatus,
}

// ─────────────────────────────────────────────────────────────────────────────
// Achievements (§23)
// ─────────────────────────────────────────────────────────────────────────────

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct AchievementSpec {
    pub id: String,
    pub name: String,
    /// Trigger condition over the context (event_type = "*").
    pub condition: crate::Condition,
    #[serde(default)]
    pub hidden: bool,
    #[serde(default)]
    pub repeatable: bool,
    #[serde(default, deserialize_with = "crate::null_vec::deserialize")]
    pub rewards: Vec<ChallengeReward>,
    #[serde(default)]
    pub status: ObjectStatus,
}

// ─────────────────────────────────────────────────────────────────────────────
// Progression (§20)
// ─────────────────────────────────────────────────────────────────────────────

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(from = "LevelTrackWire", into = "LevelTrackWire")]
pub struct LevelTrack {
    pub id: String,
    pub name: String,
    pub model: ProgressionModel,
    #[serde(default)]
    pub status: ObjectStatus,
}

/// Progression model variants (§20). Serialization is handled by LevelTrack's
/// flat wire form; this enum is the in-memory API consumed by the progression
/// crate.
#[derive(Debug, Clone, PartialEq)]
pub enum ProgressionModel {
    Linear {
        xp_per_level: i64,
    },
    Exponential {
        base: i64,
        factor: f64,
    },
    Formula {
        expression: String,
    },
}

// LevelTrack (de)serializes from the authored FLAT shape: the model kind is
// a sibling string field and the parameters live at the track level —
// {"model":"linear","xp_per_level":100} — so the conversion happens at the
// struct level, not on the model field.

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
struct LevelTrackWire {
    #[serde(default)]
    id: String,
    #[serde(default)]
    name: String,
    #[serde(default)]
    model: ModelKind,
    #[serde(default)]
    xp_per_level: i64,
    #[serde(default)]
    base: i64,
    #[serde(default)]
    factor: f64,
    #[serde(default)]
    expression: String,
    #[serde(default)]
    status: ObjectStatus,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, Default)]
#[serde(rename_all = "lowercase")]
enum ModelKind {
    #[default]
    Linear,
    Exponential,
    Formula,
}

impl From<LevelTrackWire> for LevelTrack {
    fn from(w: LevelTrackWire) -> Self {
        let model = match w.model {
            ModelKind::Linear => ProgressionModel::Linear { xp_per_level: w.xp_per_level },
            ModelKind::Exponential => ProgressionModel::Exponential { base: w.base, factor: w.factor },
            ModelKind::Formula => ProgressionModel::Formula { expression: w.expression },
        };
        LevelTrack { id: w.id, name: w.name, model, status: w.status }
    }
}

impl From<LevelTrack> for LevelTrackWire {
    fn from(t: LevelTrack) -> Self {
        let (model, xp_per_level, base, factor, expression) = match t.model {
            ProgressionModel::Linear { xp_per_level } => (ModelKind::Linear, xp_per_level, 0, 0.0, String::new()),
            ProgressionModel::Exponential { base, factor } => (ModelKind::Exponential, 0, base, factor, String::new()),
            ProgressionModel::Formula { expression } => (ModelKind::Formula, 0, 0, 0.0, expression),
        };
        LevelTrackWire {
            id: t.id,
            name: t.name,
            model,
            xp_per_level,
            base,
            factor,
            expression,
            status: t.status,
        }
    }
}

// ─────────────────────────────────────────────────────────────────────────────
// Economy (§27)
// ─────────────────────────────────────────────────────────────────────────────

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct CurrencyDef {
    pub id: String,
    pub name: String,
    /// Maximum balance a wallet may hold (0 = uncapped).
    #[serde(default)]
    pub cap: i64,
    /// Whether balances may go negative.
    #[serde(default)]
    pub allow_negative: bool,
    #[serde(default)]
    pub status: ObjectStatus,
}

// ─────────────────────────────────────────────────────────────────────────────
// Leaderboards (§29, §206)
// ─────────────────────────────────────────────────────────────────────────────

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct LeaderboardSpec {
    pub id: String,
    pub name: String,
    pub direction: Direction,
    /// Determistic tie-breaker policy.
    #[serde(default)]
    pub tie_breaker: TieBreaker,
    /// What the board tracks (§29): xp on a track (default), a currency
    /// balance, or a raw action-driven score.
    #[serde(default)]
    pub metric: LeaderboardMetric,
    /// The XP track or currency id the metric is measured on. Defaults to
    /// the "default" track (and is ignored for raw score boards).
    #[serde(default)]
    pub track: String,
    /// Optional window (rolling/seasonal leaderboards).
    #[serde(default)]
    pub window: Option<TimeWindow>,
    #[serde(default)]
    pub status: ObjectStatus,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, Default)]
#[serde(rename_all = "lowercase")]
pub enum LeaderboardMetric {
    #[default]
    Xp,
    Currency,
    Score,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, Default)]
#[serde(rename_all = "lowercase")]
pub enum Direction {
    #[default]
    Highest,
    Lowest,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, Default)]
#[serde(rename_all = "snake_case")]
pub enum TieBreaker {
    /// score desc, achieved_at asc, stable user id (default per §206).
    #[default]
    EarliestAchievedThenUserId,
    LatestAchievedThenUserId,
    UserIdOnly,
}

// ─────────────────────────────────────────────────────────────────────────────
// Workflows (§16)
// ─────────────────────────────────────────────────────────────────────────────

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct WorkflowDef {
    pub id: String,
    pub name: String,
    pub trigger: WorkflowTrigger,
    #[serde(default, deserialize_with = "crate::null_vec::deserialize")]
    pub steps: Vec<WorkflowStepDef>,
    #[serde(default)]
    pub status: ObjectStatus,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum WorkflowTrigger {
    Event { event_type: String },
    Manual,
    Schedule { cron: String },
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum WorkflowStepDef {
    Condition {
        condition: crate::Condition,
    },
    Action {
        action: ActionDef,
    },
    Delay {
        seconds: i64,
    },
    CompleteChallenge {
        challenge: String,
    },
}

// ─────────────────────────────────────────────────────────────────────────────
// Engine config bundle
// ─────────────────────────────────────────────────────────────────────────────

/// The immutable, versioned snapshot of compiled configuration the engine
/// evaluates against (§127 config snapshots).
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct EngineConfig {
    #[serde(default)]
    pub version: i64,
    #[serde(default)]
    pub project_id: String,
    #[serde(default)]
    pub environment_id: String,
    #[serde(default, deserialize_with = "crate::null_vec::deserialize")]
    pub rules: Vec<RuleDef>,
    #[serde(default, deserialize_with = "crate::null_vec::deserialize")]
    pub challenges: Vec<ChallengeSpec>,
    #[serde(default, deserialize_with = "crate::null_vec::deserialize")]
    pub streaks: Vec<StreakSpec>,
    #[serde(default, deserialize_with = "crate::null_vec::deserialize")]
    pub achievements: Vec<AchievementSpec>,
    #[serde(default, deserialize_with = "crate::null_vec::deserialize")]
    pub level_tracks: Vec<LevelTrack>,
    #[serde(default, deserialize_with = "crate::null_vec::deserialize")]
    pub currencies: Vec<CurrencyDef>,
    #[serde(default, deserialize_with = "crate::null_vec::deserialize")]
    pub leaderboards: Vec<LeaderboardSpec>,
    #[serde(default, deserialize_with = "crate::null_vec::deserialize")]
    pub workflows: Vec<WorkflowDef>,
}

impl EngineConfig {
    pub fn find_rule(&self, id: &str) -> Option<&RuleDef> {
        self.rules.iter().find(|r| r.id == id)
    }
    pub fn find_challenge(&self, id: &str) -> Option<&ChallengeSpec> {
        self.challenges.iter().find(|c| c.id == id)
    }
    pub fn find_streak(&self, id: &str) -> Option<&StreakSpec> {
        self.streaks.iter().find(|s| s.id == id)
    }
    pub fn find_currency(&self, id: &str) -> Option<&CurrencyDef> {
        self.currencies.iter().find(|c| c.id == id)
    }
    pub fn find_leaderboard(&self, id: &str) -> Option<&LeaderboardSpec> {
        self.leaderboards.iter().find(|l| l.id == id)
    }
    pub fn find_level_track(&self, id: &str) -> Option<&LevelTrack> {
        self.level_tracks.iter().find(|t| t.id == id)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn engine_config_deserializes_go_shape() {
        let raw = r#"{
          "version": 3, "project_id": "p1", "environment_id": "development",
          "rules": [{
            "id": "r1", "name": "award", "event_type": "lesson.completed",
            "condition": {"type":"eq","field":"event.type","value":"lesson.completed"},
            "actions": [{"type":"award_xp","track":"default","amount":100}],
            "status": "published"
          }],
          "currencies": [{
            "id": "coin", "name": "Coins", "cap": 0, "allow_negative": false, "status": "published"
          }],
          "challenges": null, "streaks": null, "achievements": null,
          "level_tracks": null, "leaderboards": null, "workflows": null
        }"#;
        let cfg: EngineConfig = serde_json::from_str(raw).unwrap();
        assert_eq!(cfg.rules.len(), 1);
        assert_eq!(cfg.currencies.len(), 1);
        assert!(cfg.challenges.is_empty());
        assert_eq!(cfg.rules[0].actions.len(), 1);
    }
}
