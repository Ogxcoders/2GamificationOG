//! Challenge progression semantics (§21): auto-progress via progress_event_type,
//! window-scoped instances, completion + reward grant identities (§205).

use crate::state::{ActorState, ChallengeProgress, ChallengeStatus};
use platform_common::config::ChallengeSpec;
use platform_common::{EngineConfig, TimeWindow};

/// Resolve the window key for a challenge at the evaluation instant.
pub fn window_key_for(spec: &ChallengeSpec, instant: chrono::DateTime<chrono::Utc>, tz: &str) -> String {
    match &spec.window {
        Some(w) => w.resolve(instant, tz).key,
        None => "ever".into(),
    }
}

/// Progress outcome for one challenge after an event.
#[derive(Debug, Clone, PartialEq, serde::Serialize, serde::Deserialize)]
pub struct ChallengeOutcome {
    pub challenge_id: String,
    pub progress_before: i64,
    pub progress_after: i64,
    pub status_before: ChallengeStatus,
    pub status_after: ChallengeStatus,
    pub completed: bool,
    /// Reward grant identities issued by this progression (§205 unique grants).
    #[serde(default)]
    pub reward_grants: Vec<String>,
    pub window_key: String,
}

/// Whether this event auto-progresses the challenge (§21 progress source).
pub fn event_progresses(spec: &ChallengeSpec, event_type: &str) -> bool {
    !spec.progress_event_type.is_empty() && spec.progress_event_type == event_type
}

/// Apply progress to the challenge instance. Deterministic; mutates state.
pub fn apply_progress(
    spec: &ChallengeSpec,
    state: &mut ActorState,
    delta: i64,
    instant: chrono::DateTime<chrono::Utc>,
    tz: &str,
) -> ChallengeOutcome {
    let key = window_key_for(spec, instant, tz);
    let entry = state
        .challenges
        .entry(spec.id.clone())
        .or_insert_with(|| ChallengeProgress {
            challenge_id: spec.id.clone(),
            progress: 0,
            target: spec.target,
            status: if spec.trigger_event.is_empty() {
                ChallengeStatus::InProgress
            } else {
                ChallengeStatus::Available
            },
            window_key: key.clone(),
            completions: 0,
            granted_rewards: Vec::new(),
        });

    let progress_before = entry.progress;
    let status_before = entry.status;

    // Window rollover: a new window resets progress (repeatable challenges).
    if entry.window_key != key {
        if entry.status == ChallengeStatus::Completed {
            entry.completions += 1;
        }
        entry.progress = 0;
        entry.window_key = key.clone();
        entry.status = ChallengeStatus::InProgress;
        // Window-scoped rewards may re-arm; grant identities include window key.
        entry.granted_rewards.clear();
    }

    if entry.status == ChallengeStatus::Completed {
        return ChallengeOutcome {
            challenge_id: spec.id.clone(),
            progress_before,
            progress_after: entry.progress,
            status_before,
            status_after: entry.status,
            completed: false,
            reward_grants: Vec::new(),
            window_key: key,
        };
    }

    if entry.status == ChallengeStatus::Available {
        entry.status = ChallengeStatus::InProgress;
    }

    entry.progress += delta;
    if spec.target > 0 && entry.progress >= spec.target {
        entry.progress = spec.target;
        entry.status = ChallengeStatus::Completed;
    }

    let completed = entry.status == ChallengeStatus::Completed;

    // Issue reward grant identities for newly completed challenges.
    let mut grants = Vec::new();
    if completed {
        for (i, r) in spec.rewards.iter().enumerate() {
            let grant = format!("challenge:{}:{}:{}:{}", spec.id, key, i, describe_reward(r));
            if !entry.granted_rewards.contains(&grant) {
                entry.granted_rewards.push(grant.clone());
                grants.push(grant);
            }
        }
    }

    ChallengeOutcome {
        challenge_id: spec.id.clone(),
        progress_before,
        progress_after: entry.progress,
        status_before,
        status_after: entry.status,
        completed,
        reward_grants: grants,
        window_key: key,
    }
}

/// Force-complete (admin / rule action).
pub fn force_complete(
    spec: &ChallengeSpec,
    state: &mut ActorState,
    instant: chrono::DateTime<chrono::Utc>,
    tz: &str,
) -> ChallengeOutcome {
    apply_progress(
        spec,
        state,
        spec.target.max(i64::MAX / 2),
        instant,
        tz,
    )
}

fn describe_reward(r: &platform_common::config::ChallengeReward) -> String {
    format!("{:?}:{}:{}", r.kind, r.target, r.amount)
}
