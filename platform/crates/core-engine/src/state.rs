//! Engine-side actor state: the snapshot the control plane loads for one user
//! before processing an event, and persists after applying commands.

use platform_common::config::EngineConfig;
use platform_progression::TrackState;
use platform_rules::RuleState;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;

/// Runtime state of one actor in one environment.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct ActorState {
    pub user_id: String,
    /// track id -> state.
    #[serde(default)]
    pub tracks: HashMap<String, TrackState>,
    /// Rule cooldowns/frequency counters.
    #[serde(default)]
    pub rule_state: RuleState,
    /// challenge id -> current/last instance state.
    #[serde(default)]
    pub challenges: HashMap<String, ChallengeProgress>,
    /// streak id -> state.
    #[serde(default)]
    pub streaks: HashMap<String, StreakState>,
    /// achievement id -> unlocked epoch ms.
    #[serde(default)]
    pub achievements: HashMap<String, i64>,
    /// leaderboard id -> score.
    #[serde(default)]
    pub leaderboards: HashMap<String, i64>,
    /// Variables set by SetVariable actions.
    #[serde(default)]
    pub variables: HashMap<String, serde_json::Value>,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ChallengeProgress {
    pub challenge_id: String,
    pub progress: i64,
    pub target: i64,
    pub status: ChallengeStatus,
    /// Window key this progress belongs to (daily/weekly...).
    #[serde(default)]
    pub window_key: String,
    /// Completions across windows (repeatable challenges).
    #[serde(default)]
    pub completions: i64,
    /// Reward grant identity per §205 (unique logical grant).
    #[serde(default)]
    pub granted_rewards: Vec<String>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, Default)]
#[serde(rename_all = "lowercase")]
pub enum ChallengeStatus {
    #[default]
    Available,
    InProgress,
    Completed,
    Expired,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct StreakState {
    pub streak_id: String,
    pub current: i64,
    pub best: i64,
    /// Window key of the last qualifying action.
    pub last_window_key: String,
    /// Freezes consumed in the current period.
    #[serde(default)]
    pub freezes_used: i32,
    /// True when the streak is alive (within grace of the current window).
    #[serde(default)]
    pub active: bool,
}

impl ActorState {
    pub fn new(user_id: impl Into<String>) -> Self {
        ActorState { user_id: user_id.into(), ..Default::default() }
    }

    /// Track accessor with lazy init.
    pub fn track(&mut self, track_id: &str) -> &mut TrackState {
        self.tracks
            .entry(track_id.to_string())
            .or_insert_with(|| TrackState::new(track_id))
    }

    pub fn track_level(&self, track_id: &str) -> i64 {
        self.tracks.get(track_id).map(|t| t.level).unwrap_or(0)
    }

    pub fn track_xp(&self, track_id: &str) -> i64 {
        self.tracks.get(track_id).map(|t| t.xp).unwrap_or(0)
    }

    pub fn leaderboard_score(&self, lb: &str) -> i64 {
        self.leaderboards.get(lb).copied().unwrap_or(0)
    }
}

impl ActorState {
    /// Build the evaluation context values map for this actor (§12):
    /// user.*, metrics.* (derived), variables.*.
    pub fn context_values(&self, cfg: &EngineConfig) -> HashMap<String, serde_json::Value> {
        let mut m = HashMap::new();
        m.insert("user.id".into(), serde_json::json!(self.user_id));

        // Level/XP per track, with the first/`default` track surfaced as user.level.
        let default_track = cfg
            .level_tracks
            .iter()
            .find(|t| t.id == "default")
            .or_else(|| cfg.level_tracks.first());
        if let Some(t) = default_track {
            m.insert("user.level".into(), serde_json::json!(self.track_level(&t.id)));
            m.insert("user.xp".into(), serde_json::json!(self.track_xp(&t.id)));
        }
        for t in &cfg.level_tracks {
            m.insert(format!("user.{}.level", t.id), serde_json::json!(self.track_level(&t.id)));
            m.insert(format!("user.{}.xp", t.id), serde_json::json!(self.track_xp(&t.id)));
        }

        // Challenge progress surfaced.
        for c in &cfg.challenges {
            if let Some(p) = self.challenges.get(&c.id) {
                m.insert(format!("challenge.{}.progress", c.id), serde_json::json!(p.progress));
                m.insert(format!("challenge.{}.completed", c.id), serde_json::json!(p.status == ChallengeStatus::Completed));
            }
        }

        // Streaks.
        for s in &cfg.streaks {
            if let Some(st) = self.streaks.get(&s.id) {
                m.insert(format!("streak.{}.current", s.id), serde_json::json!(st.current));
                m.insert(format!("streak.{}.best", s.id), serde_json::json!(st.best));
            }
        }

        // Leaderboard scores.
        for l in &cfg.leaderboards {
            m.insert(format!("leaderboard.{}.score", l.id), serde_json::json!(self.leaderboard_score(&l.id)));
        }

        // Achievements unlocked flags.
        for a in &cfg.achievements {
            m.insert(
                format!("achievement.{}.unlocked", a.id),
                serde_json::json!(self.achievements.contains_key(&a.id)),
            );
        }

        // Variables.
        for (k, v) in &self.variables {
            m.insert(format!("variables.{}", k), v.clone());
        }

        m
    }
}
