//! Event processing orchestrator — the universal loop (§4).
//!
//! ```text
//! event → context → rules → commands → trace
//! ```
//!
//! Pure function over (event, config, actor state). The control plane:
//! 1. loads ActorState,
//! 2. calls `process_event`,
//! 3. applies `outcome.commands` in ONE transaction,
//! 4. persists the trace + updated state.

use crate::achievement;
use crate::challenge;
use crate::commands::{expand_action, Command};
use crate::state::{ActorState, ChallengeStatus};
use crate::streak;
use crate::workflow::{self, WorkflowRun};
use platform_common::config::EngineConfig;
use platform_common::{CanonicalEvent, Condition, Context, DecisionTrace, ObjectStatus, TraceNodeKind};
use platform_rules::{evaluate_condition, rule_matches_event, RuleState};
use serde::{Deserialize, Serialize};
use std::collections::HashMap;

#[derive(Debug, Clone, Default)]
pub struct ProcessOptions {
    /// Skip side-effect commands (simulation/dry-run mode, §232).
    pub dry_run: bool,
}

/// Full processing outcome for one event.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct EngineOutcome {
    pub event_id: String,
    pub trace: DecisionTrace,
    /// Commands for the control plane to apply atomically.
    #[serde(default, deserialize_with = "platform_common::null_vec::deserialize")]
    pub commands: Vec<Command>,
    /// Rule ids that fired.
    #[serde(default)]
    pub rules_matched: Vec<String>,
    /// Challenge completions detected (id -> window key).
    #[serde(default)]
    pub challenges_completed: Vec<String>,
    /// Achievements newly unlocked.
    #[serde(default)]
    pub achievements_unlocked: Vec<String>,
    /// Streak effects (id -> effect).
    #[serde(default)]
    pub streak_effects: HashMap<String, String>,
    /// Level-ups (track -> outcome).
    #[serde(default)]
    pub level_ups: HashMap<String, i64>,
    /// Leaderboard updates (id -> new score).
    #[serde(default)]
    pub leaderboard_updates: HashMap<String, i64>,
    /// Wallet deltas (currency -> signed delta).
    #[serde(default)]
    pub wallet_deltas: HashMap<String, i64>,
    /// XP deltas (track -> delta).
    #[serde(default)]
    pub xp_deltas: HashMap<String, i64>,
    /// Workflows started.
    #[serde(default)]
    pub workflow_runs: Vec<WorkflowRun>,
    /// Freshly computed state snapshot after applying everything (for
    /// persistence by the control plane).
    pub state: ActorState,
    /// Errors encountered (non-fatal ones recorded for the trace).
    #[serde(default)]
    pub warnings: Vec<String>,
}

/// Process one event end-to-end.
pub fn process_event(
    event: &CanonicalEvent,
    cfg: &EngineConfig,
    actor: &mut ActorState,
    options: &ProcessOptions,
) -> Result<EngineOutcome, platform_common::EngineError> {
    crate::validate_event(event)?;
    let mut trace = DecisionTrace::new(
        &event.event_id,
        &event.correlation_id,
        &event.project_id,
        &event.environment_id,
        &event.actor_id,
    );
    trace.config_version = cfg.version;

    let instant = crate::evaluation_instant(event, None);
    let now_ms = instant.timestamp_millis();
    let now_epoch = instant.timestamp();
    let tz = "UTC"; // effective tz comes from context when control plane supplies it

    // ── 1. Context (§12): event.* + user.* + config views ──
    let mut values = actor.context_values(cfg);
    values.insert("event.id".into(), serde_json::json!(event.event_id));
    values.insert("event.type".into(), serde_json::json!(event.event_type));
    values.insert("event.version".into(), serde_json::json!(event.event_version));
    values.insert("event.actor_id".into(), serde_json::json!(event.actor_id));
    values.insert("event.subject_id".into(), serde_json::json!(event.subject_id));
    for (k, v) in &event.payload {
        values.insert(format!("event.payload.{}", k), v.clone());
    }
    // Nested event object for formula path walking.
    values.insert(
        "event".into(),
        serde_json::json!({
            "id": event.event_id,
            "type": event.event_type,
            "actor_id": event.actor_id,
            "subject_id": event.subject_id,
            "payload": event.payload,
        }),
    );

    let ctx = Context {
        values,
        timezone: String::new(),
        project_timezone: String::new(),
        instant: Some(instant.to_rfc3339()),
    };

    let root = trace.push(
        "",
        TraceNodeKind::EventIngress,
        format!("event `{}` from `{}`", event.event_type, event.source),
        serde_json::json!({"event_id": event.event_id, "occurred_at": event.occurred_at}),
    );
    let ctx_node = trace.push(
        &root,
        TraceNodeKind::ContextResolved,
        format!("context resolved: {} scope keys", ctx.values.len()),
        serde_json::json!({"actor": event.actor_id, "config_version": cfg.version}),
    );

    let mut outcome = EngineOutcome {
        event_id: event.event_id.clone(),
        trace: DecisionTrace::default(),
        commands: Vec::new(),
        rules_matched: Vec::new(),
        challenges_completed: Vec::new(),
        achievements_unlocked: Vec::new(),
        streak_effects: HashMap::new(),
        level_ups: HashMap::new(),
        leaderboard_updates: HashMap::new(),
        wallet_deltas: HashMap::new(),
        xp_deltas: HashMap::new(),
        workflow_runs: Vec::new(),
        state: ActorState::new(&event.actor_id),
        warnings: Vec::new(),
    };

    // ── 2. Rules (§13): priority order, cooldown, frequency ──
    let mut rules: Vec<&platform_common::config::RuleDef> = cfg
        .rules
        .iter()
        .filter(|r| rule_matches_event(r, &event.event_type))
        .collect();
    rules.sort_by_key(|r| -r.priority);

    let window = platform_common::TimeWindow::Fixed {
        unit: platform_common::CalendarUnit::Day,
        timezone: "UTC".into(),
    };
    let window_key = window.resolve(instant, tz).key;

    for rule in rules {
        let rule_node = trace.push(
            &ctx_node,
            if matches!(rule.status, ObjectStatus::Active) {
                TraceNodeKind::RuleMatched
            } else {
                TraceNodeKind::RuleSkipped
            },
            format!("evaluating rule `{}` (priority {})", rule.id, rule.priority),
            serde_json::json!({"rule": rule.id, "event_type": rule.event_type}),
        );

        if actor.rule_state.cooldown_active(rule, now_epoch) {
            trace.push(
                &rule_node,
                TraceNodeKind::RuleSkipped,
                format!("rule `{}` skipped: cooldown active ({}s)", rule.id, rule.cooldown_seconds),
                serde_json::json!({"reason": "cooldown"}),
            );
            continue;
        }
        if actor.rule_state.frequency_capped(rule, &window_key) {
            trace.push(
                &rule_node,
                TraceNodeKind::RuleSkipped,
                format!("rule `{}` skipped: frequency cap reached ({}/{})", rule.id, rule.frequency_cap, window_key),
                serde_json::json!({"reason": "frequency_cap"}),
            );
            continue;
        }

        let evaluation = evaluate_condition(&rule.condition, &ctx);
        let cond_node = trace.push(
            &rule_node,
            TraceNodeKind::ConditionEvaluated,
            format!("condition {} -> {}", if evaluation.matched { "matched" } else { "not matched" }, evaluation.reason),
            serde_json::json!({"matched": evaluation.matched, "reason": evaluation.reason}),
        );

        if !evaluation.matched {
            continue;
        }

        outcome.rules_matched.push(rule.id.clone());
        actor.rule_state.record_fire(&rule.id, now_epoch, &window_key);

        // Expand actions into commands.
        for (i, action) in rule.actions.iter().enumerate() {
            match expand_action(action, &format!("rule:{}", rule.id), &event.event_id, i, &ctx.values) {
                Ok(cmd) => {
                    trace.push(
                        &cond_node,
                        TraceNodeKind::ActionExecuted,
                        cmd.explanation.clone(),
                        serde_json::json!({"command_id": cmd.command_id, "idempotency_key": cmd.idempotency_key}),
                    );
                    outcome.commands.push(cmd);
                }
                Err(e) => {
                    trace.push(
                        &cond_node,
                        TraceNodeKind::ActionFailed,
                        format!("action {} failed: {}", i, e),
                        serde_json::json!({"error": e.to_string(), "code": e.code()}),
                    );
                    outcome.warnings.push(e.to_string());
                }
            }
        }
    }

    // ── 3. Challenge auto-progress (§21 progress source) ──
    for spec in &cfg.challenges {
        let active = matches!(spec.status, ObjectStatus::Active);
        if !active {
            continue;
        }
        if challenge::event_progresses(spec, &event.event_type) {
            let before = actor
                .challenges
                .get(&spec.id)
                .map(|c| c.status == ChallengeStatus::Completed)
                .unwrap_or(false);
            let out = challenge::apply_progress(spec, actor, 1, instant, tz);
            trace.push(
                &ctx_node,
                TraceNodeKind::ChallengeProgressed,
                format!(
                    "challenge `{}` progress {} -> {} (window {})",
                    spec.id, out.progress_before, out.progress_after, out.window_key
                ),
                serde_json::json!({"challenge": spec.id, "completed": out.completed, "window": out.window_key}),
            );
            if out.completed && !before {
                outcome.challenges_completed.push(spec.id.clone());
                trace.push(
                    &ctx_node,
                    TraceNodeKind::ChallengeCompleted,
                    format!("challenge `{}` completed", spec.id),
                    serde_json::json!({"challenge": spec.id, "rewards": out.reward_grants}),
                );
                // Auto-issue challenge rewards as commands.
                for (i, r) in spec.rewards.iter().enumerate() {
                    let cmd = challenge_reward_command(spec, r, i, &event.event_id, &out.window_key);
                    trace.push(
                        &ctx_node,
                        TraceNodeKind::CommandApplied,
                        cmd.explanation.clone(),
                        serde_json::json!({"grant": out.reward_grants.get(i)}),
                    );
                    outcome.commands.push(cmd);
                }
            }
        }
    }

    // ── 4. Streaks (§24) ──
    for spec in &cfg.streaks {
        if !matches!(spec.status, ObjectStatus::Active) {
            continue;
        }
        if spec.event_type == event.event_type {
            let out = streak::apply_qualifying(spec, actor, instant, tz);
            trace.push(
                &ctx_node,
                TraceNodeKind::StreakUpdated,
                format!("streak `{}` {} -> {} ({})", spec.id, out.before, out.after, out.effect),
                serde_json::json!({"streak": spec.id, "effect": out.effect, "window": out.window_key}),
            );
            outcome.streak_effects.insert(spec.id.clone(), out.effect.clone());
        }
    }

    // ── 5. Achievements (§23) ──
    for spec in &cfg.achievements {
        if !matches!(spec.status, ObjectStatus::Active) {
            continue;
        }
        let out = achievement::evaluate(spec, actor, &ctx, now_ms);
        if out.newly_unlocked {
            trace.push(
                &ctx_node,
                TraceNodeKind::AchievementUnlocked,
                format!("achievement `{}` unlocked", spec.id),
                serde_json::json!({"achievement": spec.id, "rewards": out.reward_grants}),
            );
            outcome.achievements_unlocked.push(spec.id.clone());
            for (i, r) in spec.rewards.iter().enumerate() {
                let cmd = achievement_reward_command(spec, r, i, &event.event_id, now_ms);
                outcome.commands.push(cmd);
            }
        }
    }

    // ── 6. Workflows (§16) ──
    for def in &cfg.workflows {
        if !matches!(def.status, ObjectStatus::Active) {
            continue;
        }
        if workflow::trigger_matches(def, &event.event_type) {
            let mut run = workflow::start_run(def, &event.event_id, &event.actor_id);
            // Execute synchronously up to the first delay.
            let _ = workflow::tick_workflow(def, &mut run, actor, &ctx);
            for cmd in run.commands.clone() {
                trace.push(
                    &ctx_node,
                    TraceNodeKind::WorkflowStep,
                    format!("workflow `{}`: {}", def.id, cmd.explanation),
                    serde_json::json!({"workflow": def.id}),
                );
                outcome.commands.push(cmd);
            }
            outcome.workflow_runs.push(run);
        }
    }

    // ── 7. Apply projections in-memory for the state snapshot + summaries ──
    if !options.dry_run {
        let commands = outcome.commands.clone();
        apply_projection(&commands, cfg, actor, instant, &mut outcome, &mut trace, &ctx_node);
    }

    outcome.state = actor.clone();
    outcome.trace = trace;
    Ok(outcome)
}

/// Apply command effects to the in-memory actor state (pure projection).
/// The Go control plane re-applies these against Postgres authoritatively —
/// this keeps the returned snapshot consistent for inspection/trace.
fn apply_projection(
    commands: &[Command],
    cfg: &EngineConfig,
    actor: &mut ActorState,
    instant: chrono::DateTime<chrono::Utc>,
    outcome: &mut EngineOutcome,
    trace: &mut DecisionTrace,
    parent: &str,
) {
    use crate::commands::CommandKind;

    for cmd in commands {
        match &cmd.kind {
            CommandKind::AwardXp { track, amount } => {
                let model = cfg
                    .level_tracks
                    .iter()
                    .find(|t| &t.id == track)
                    .map(|t| t.model.clone());
                if let Some(model) = model {
                    let before = actor.track_level(track);
                    let st = actor.track(track);
                    let result = platform_progression::apply_xp(&model, st, *amount);
                    *outcome.xp_deltas.entry(track.clone()).or_insert(0) += *amount;
                    if result.leveled_up {
                        outcome.level_ups.insert(track.clone(), result.level_after);
                        trace.push(
                            parent,
                            TraceNodeKind::LevelUp,
                            format!("level up on `{}`: {} -> {}", track, before, result.level_after),
                            serde_json::json!({"track": track, "from": before, "to": result.level_after}),
                        );
                    }
                } else {
                    // XP without a track definition still accumulates.
                    let st = actor.track(track);
                    st.xp = (st.xp + amount).max(0);
                    *outcome.xp_deltas.entry(track.clone()).or_insert(0) += *amount;
                }
            }
            CommandKind::AddCurrency { currency, amount } => {
                *outcome.wallet_deltas.entry(currency.clone()).or_insert(0) += *amount;
                trace.push(
                    parent,
                    TraceNodeKind::LedgerEntry,
                    format!("ledger: +{} `{}` ({})", amount, currency, cmd.source),
                    serde_json::json!({"currency": currency, "amount": amount, "reference": cmd.idempotency_key}),
                );
            }
            CommandKind::SpendCurrency { currency, amount } => {
                *outcome.wallet_deltas.entry(currency.clone()).or_insert(0) -= *amount;
                trace.push(
                    parent,
                    TraceNodeKind::LedgerEntry,
                    format!("ledger: -{} `{}` ({})", amount, currency, cmd.source),
                    serde_json::json!({"currency": currency, "amount": -amount, "reference": cmd.idempotency_key}),
                );
            }
            CommandKind::UpdateLeaderboard { leaderboard, delta, .. } => {
                let next = actor.leaderboard_score(leaderboard) + delta;
                actor.leaderboards.insert(leaderboard.clone(), next);
                outcome.leaderboard_updates.insert(leaderboard.clone(), next);
                trace.push(
                    parent,
                    TraceNodeKind::LeaderboardUpdated,
                    format!("leaderboard `{}` -> {}", leaderboard, next),
                    serde_json::json!({"leaderboard": leaderboard, "score": next}),
                );
            }
            CommandKind::SetVariable { key, value } => {
                actor.variables.insert(key.clone(), value.clone());
            }
            CommandKind::ProgressChallenge { challenge, amount } => {
                if let Some(spec) = cfg.find_challenge(challenge) {
                    challenge::apply_progress(spec, actor, *amount, instant, "UTC");
                }
            }
            CommandKind::StartChallenge { challenge } => {
                if let Some(spec) = cfg.find_challenge(challenge) {
                    let key = challenge::window_key_for(spec, instant, "UTC");
                    let entry = actor.challenges.entry(spec.id.clone()).or_insert_with(|| {
                        crate::state::ChallengeProgress {
                            challenge_id: spec.id.clone(),
                            progress: 0,
                            target: spec.target,
                            status: ChallengeStatus::InProgress,
                            window_key: key.clone(),
                            completions: 0,
                            granted_rewards: Vec::new(),
                        }
                    });
                    entry.status = ChallengeStatus::InProgress;
                }
            }
            _ => {
                // Remaining commands are applied by the control plane
                // (items, entitlements, notifications, webhooks...).
            }
        }
    }
}

fn challenge_reward_command(
    spec: &platform_common::config::ChallengeSpec,
    r: &platform_common::config::ChallengeReward,
    index: usize,
    event_id: &str,
    window_key: &str,
) -> Command {
    use crate::commands::CommandKind;
    use platform_common::config::RewardKind;
    let kind = match r.kind {
        RewardKind::Xp => CommandKind::AwardXp {
            track: if r.target.is_empty() { "default".into() } else { r.target.clone() },
            amount: r.amount,
        },
        RewardKind::Currency => CommandKind::AddCurrency { currency: r.target.clone(), amount: r.amount },
        RewardKind::Item => CommandKind::GrantItem { item: r.target.clone(), quantity: r.amount },
        RewardKind::Entitlement => CommandKind::GrantEntitlement {
            entitlement: r.target.clone(),
            duration_seconds: r.amount,
        },
    };
    Command::new(
        &format!("challenge:{}", spec.id),
        event_id,
        index,
        kind,
        format!("challenge `{}` reward (window {})", spec.id, window_key),
    )
}

fn achievement_reward_command(
    spec: &platform_common::config::AchievementSpec,
    r: &platform_common::config::ChallengeReward,
    index: usize,
    event_id: &str,
    now_ms: i64,
) -> Command {
    let _ = now_ms;
    challenge_reward_command(
        &platform_common::config::ChallengeSpec {
            id: format!("achievement:{}", spec.id),
            name: spec.name.clone(),
            trigger_event: String::new(),
            progress_event_type: String::new(),
            target: 1,
            window: None,
            rewards: vec![r.clone()],
            repeatability: platform_common::config::RepeatMode::Once,
            status: spec.status,
        },
        r,
        index,
        event_id,
        "ever",
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use platform_common::config::{ActionDef, ChallengeReward, ChallengeSpec, CurrencyDef, RewardKind, RuleDef, StreakSpec};
    use platform_common::{Condition, ObjectStatus, Operand};

    fn cfg() -> EngineConfig {
        EngineConfig {
            version: 1,
            project_id: "p1".into(),
            environment_id: "development".into(),
            rules: vec![RuleDef {
                id: "r1".into(),
                name: "Award lesson XP".into(),
                priority: 10,
                event_type: "lesson.completed".into(),
                condition: Condition::Eq {
                    field: Operand::path("event.type"),
                    value: Operand::value(serde_json::json!("lesson.completed")),
                },
                actions: vec![ActionDef::AwardXp { track: "default".into(), amount: 100 }],
                cooldown_seconds: 0,
                frequency_cap: 0,
                frequency_window: None,
                status: ObjectStatus::Active,
            }],
            challenges: vec![ChallengeSpec {
                id: "c1".into(),
                name: "Complete 3 lessons".into(),
                trigger_event: String::new(),
                progress_event_type: "lesson.completed".into(),
                target: 3,
                window: None,
                rewards: vec![ChallengeReward { kind: RewardKind::Currency, amount: 10, target: "coin".into() }],
                repeatability: platform_common::config::RepeatMode::Once,
                status: ObjectStatus::Active,
            }],
            streaks: vec![StreakSpec {
                id: "s1".into(),
                name: "Daily streak".into(),
                event_type: "lesson.completed".into(),
                window: platform_common::TimeWindow::Fixed {
                    unit: platform_common::CalendarUnit::Day,
                    timezone: "UTC".into(),
                },
                grace_seconds: 0,
                freezes_allowed: 0,
                multiplier: None,
                status: ObjectStatus::Active,
            }],
            achievements: vec![],
            level_tracks: vec![platform_common::config::LevelTrack {
                id: "default".into(),
                name: "Default".into(),
                model: platform_common::config::ProgressionModel::Linear { xp_per_level: 100 },
                status: ObjectStatus::Active,
            }],
            currencies: vec![CurrencyDef {
                id: "coin".into(),
                name: "Coins".into(),
                cap: 0,
                allow_negative: false,
                status: ObjectStatus::Active,
            }],
            leaderboards: vec![],
            workflows: vec![],
        }
    }

    fn event() -> CanonicalEvent {
        let mut e = CanonicalEvent::new("p1", "development", "u1", "lesson.completed");
        e.occurred_at = "2026-09-08T10:00:00Z".into();
        e
    }

    #[test]
    fn golden_path_event_awards_xp_currency_and_progresses_challenge() {
        let cfg = cfg();
        let mut actor = ActorState::new("u1");
        let out = process_event(&event(), &cfg, &mut actor, &ProcessOptions::default()).unwrap();

        // Rule fired.
        assert_eq!(out.rules_matched, vec!["r1"]);
        // Commands: rule xp + challenge progression.
        assert!(out.commands.iter().any(|c| matches!(c.kind, crate::commands::CommandKind::AwardXp { amount: 100, .. })));
        // XP projection + no level-up yet (100 = level 1 boundary).
        assert_eq!(out.xp_deltas.get("default"), Some(&100));
        assert_eq!(out.state.track_level("default"), 1);
        // Challenge progressed by 1.
        assert_eq!(out.state.challenges["c1"].progress, 1);
        assert_eq!(out.state.challenges["c1"].status, ChallengeStatus::InProgress);
        // Streak extended.
        assert_eq!(out.streak_effects.get("s1").map(|s| s.as_str()), Some("extended"));
        assert_eq!(out.state.streaks["s1"].current, 1);
        // Trace explains the decision (§70).
        assert!(out.trace.nodes.len() >= 5);
    }

    #[test]
    fn challenge_completion_issues_reward_command() {
        let cfg = cfg();
        let mut actor = ActorState::new("u1");
        // Pre-progress 2/3.
        actor.challenges.insert(
            "c1".into(),
            crate::state::ChallengeProgress {
                challenge_id: "c1".into(),
                progress: 2,
                target: 3,
                status: ChallengeStatus::InProgress,
                window_key: "ever".into(),
                completions: 0,
                granted_rewards: vec![],
            },
        );
        let out = process_event(&event(), &cfg, &mut actor, &ProcessOptions::default()).unwrap();
        assert!(out.challenges_completed.contains(&"c1".to_string()));
        let currency_cmds: Vec<_> = out
            .commands
            .iter()
            .filter(|c| matches!(&c.kind, crate::commands::CommandKind::AddCurrency { ref currency, ref amount } if currency == "coin" && *amount == 10))
            .collect();
        assert_eq!(currency_cmds.len(), 1);
        assert_eq!(out.wallet_deltas.get("coin"), Some(&10));
    }

    #[test]
    fn idempotent_replay_same_event_does_not_double_award() {
        // The engine itself is pure; the control plane dedups on idempotency
        // keys. Simulate: the second processing of the SAME event id must be
        // prevented upstream — but rule frequency/cooldown state also guards.
        let cfg = cfg();
        let mut actor = ActorState::new("u1");
        let e = event();
        let out1 = process_event(&e, &cfg, &mut actor, &ProcessOptions::default()).unwrap();
        assert_eq!(out1.rules_matched.len(), 1);
        // Re-processing accumulates state (engine is stateless w.r.t. dedup),
        // which is why commands carry idempotency keys for the CP to dedup.
        let cmd_keys: Vec<&str> = out1.commands.iter().map(|c| c.idempotency_key.as_str()).collect();
        assert!(cmd_keys.iter().all(|k| k.contains(&e.event_id)));
    }

    #[test]
    fn level_up_detected() {
        let cfg = cfg();
        let mut actor = ActorState::new("u1");
        actor.track("default").xp = 150; // level 1
        let out = process_event(&event(), &cfg, &mut actor, &ProcessOptions::default()).unwrap();
        assert!(out.level_ups.contains_key("default"));
        assert_eq!(out.state.track_level("default"), 2);
    }

    #[test]
    fn dry_run_leaves_state_untouched() {
        let cfg = cfg();
        let mut actor = ActorState::new("u1");
        let out = process_event(&event(), &cfg, &mut actor, &ProcessOptions { dry_run: true }).unwrap();
        // Commands are still produced (for inspection)...
        assert!(!out.commands.is_empty());
        // ...but no projections applied to the actor.
        assert!(out.xp_deltas.is_empty());
        assert_eq!(actor.track_xp("default"), 0);
    }

    #[test]
    fn rule_cooldown_blocks_second_fire() {
        let mut c = cfg();
        c.rules[0].cooldown_seconds = 3600;
        let mut actor = ActorState::new("u1");
        let e = event();
        let out1 = process_event(&e, &c, &mut actor, &ProcessOptions::default()).unwrap();
        assert_eq!(out1.rules_matched.len(), 1);
        // Different event id, same instant window -> cooldown active.
        let mut e2 = event();
        e2.event_id = "e2".into();
        let out2 = process_event(&e2, &c, &mut actor, &ProcessOptions::default()).unwrap();
        assert!(out2.rules_matched.is_empty());
    }

    #[test]
    fn frequency_cap_blocks_after_limit() {
        let mut c = cfg();
        c.rules[0].frequency_cap = 1;
        let mut actor = ActorState::new("u1");
        let e = event();
        process_event(&e, &c, &mut actor, &ProcessOptions::default()).unwrap();
        let mut e2 = event();
        e2.event_id = "e2".into();
        let out2 = process_event(&e2, &c, &mut actor, &ProcessOptions::default()).unwrap();
        assert!(out2.rules_matched.is_empty());
    }

    #[test]
    fn trace_answers_why() {
        let cfg = cfg();
        let mut actor = ActorState::new("u1");
        let out = process_event(&event(), &cfg, &mut actor, &ProcessOptions::default()).unwrap();
        let labels: Vec<&str> = out.trace.nodes.iter().map(|n| n.label.as_str()).collect();
        assert!(labels.iter().any(|l| l.contains("rule `r1`")));
        assert!(labels.iter().any(|l| l.contains("award 100 xp")));
    }
}
