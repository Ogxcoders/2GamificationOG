//! Achievement evaluation (§23): rule-triggered unlocks, repeatable and hidden.

use crate::state::ActorState;
use platform_common::config::AchievementSpec;
use platform_common::{Condition, Context};

/// Outcome of one achievement evaluation.
#[derive(Debug, Clone, PartialEq, serde::Serialize, serde::Deserialize)]
pub struct AchievementOutcome {
    pub achievement_id: String,
    pub newly_unlocked: bool,
    pub already_unlocked: bool,
    /// Reward grant identities (§205).
    #[serde(default)]
    pub reward_grants: Vec<String>,
}

/// Evaluate an achievement against the context. Replays are no-ops for
/// one-time achievements (idempotent unlock).
pub fn evaluate(
    spec: &AchievementSpec,
    state: &mut ActorState,
    ctx: &Context,
    now_ms: i64,
) -> AchievementOutcome {
    let already = state.achievements.contains_key(&spec.id);

    if already && !spec.repeatable {
        return AchievementOutcome {
            achievement_id: spec.id.clone(),
            newly_unlocked: false,
            already_unlocked: true,
            reward_grants: Vec::new(),
        };
    }

    let cond = spec.condition.clone();
    let eval = platform_rules::evaluate_condition(&cond, ctx);
    if !eval.matched {
        return AchievementOutcome {
            achievement_id: spec.id.clone(),
            newly_unlocked: false,
            already_unlocked: false,
            reward_grants: Vec::new(),
        };
    }

    // Repeatable achievements use occurrence-counted grant identities.
    let count = state.achievements.values().filter(|v| **v > 0).count() as i64;
    let occurrence = if spec.repeatable { count } else { 0 };
    let _ = occurrence;

    state
        .achievements
        .entry(spec.id.clone())
        .and_modify(|v| *v = now_ms)
        .or_insert(now_ms);

    let mut grants = Vec::new();
    for (i, r) in spec.rewards.iter().enumerate() {
        grants.push(format!(
            "achievement:{}:{}:{:?}:{}:{}",
            spec.id,
            if spec.repeatable { now_ms } else { 0 },
            r.kind,
            r.target,
            i
        ));
    }

    AchievementOutcome {
        achievement_id: spec.id.clone(),
        newly_unlocked: true,
        already_unlocked: false,
        reward_grants: grants,
    }
}

/// Direct unlock (admin action / rule action).
pub fn force_unlock(
    spec: &AchievementSpec,
    state: &mut ActorState,
    now_ms: i64,
) -> AchievementOutcome {
    let already = state.achievements.contains_key(&spec.id);
    state.achievements.insert(spec.id.clone(), now_ms);
    let grants = if already && !spec.repeatable {
        Vec::new()
    } else {
        spec.rewards
            .iter()
            .enumerate()
            .map(|(i, r)| {
                format!(
                    "achievement:{}:{}:{:?}:{}:{}",
                    spec.id,
                    if spec.repeatable { now_ms } else { 0 },
                    r.kind,
                    r.target,
                    i
                )
            })
            .collect()
    };
    AchievementOutcome {
        achievement_id: spec.id.clone(),
        newly_unlocked: !already,
        already_unlocked: already && !spec.repeatable,
        reward_grants: grants,
    }
}
