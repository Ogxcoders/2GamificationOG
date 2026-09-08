//! Transactional command model (§15 Action Engine).
//!
//! Actions are *intent*; commands are *validated, idempotent operations* that
//! the control plane applies atomically. Every command carries a dedup
//! identity so replays are detectable (§176 idempotency model).

use platform_common::config::ActionDef;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum CommandKind {
    AwardXp { track: String, amount: i64 },
    AddCurrency { currency: String, amount: i64 },
    SpendCurrency { currency: String, amount: i64 },
    GrantItem { item: String, quantity: i64 },
    GrantReward { reward: String },
    ProgressChallenge { challenge: String, amount: i64 },
    StartChallenge { challenge: String },
    CompleteChallenge { challenge: String },
    UnlockAchievement { achievement: String },
    UpdateStreak { streak: String, window_key: String, grace_applied: bool },
    UpdateLeaderboard { leaderboard: String, delta: i64, score: i64 },
    GrantEntitlement { entitlement: String, duration_seconds: i64 },
    Notify { template: String, params: HashMap<String, String> },
    ShowPaywall { paywall: String },
    StartWorkflow { workflow: String, input: serde_json::Value },
    SetVariable { key: String, value: serde_json::Value },
    EmitEvent { event_type: String, payload: HashMap<String, serde_json::Value> },
    CallWebhook { endpoint: String },
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Command {
    /// Unique id for this command instance.
    pub command_id: String,
    /// Idempotency/dedup identity: (source_rule, event_id, action_index) or
    /// equivalent — the control plane persists this to reject replays.
    pub idempotency_key: String,
    /// Originating rule (or "system:<domain>" for direct API operations).
    pub source: String,
    pub event_id: String,
    pub kind: CommandKind,
    /// Human-readable explanation for the trace (§106).
    #[serde(default)]
    pub explanation: String,
}

impl Command {
    pub fn new(source: &str, event_id: &str, index: usize, kind: CommandKind, explanation: impl Into<String>) -> Self {
        Command {
            command_id: platform_common::new_uuid(),
            idempotency_key: format!("{}:{}:{}", source, event_id, index),
            source: source.into(),
            event_id: event_id.into(),
            kind,
            explanation: explanation.into(),
        }
    }
}

/// Expand a rule action into commands. Formula scores resolve against the
/// context; literals pass through.
pub fn expand_action(
    action: &ActionDef,
    source: &str,
    event_id: &str,
    index: usize,
    ctx: &HashMap<String, serde_json::Value>,
) -> Result<Command, platform_common::EngineError> {
    let kind = match action {
        ActionDef::AwardXp { track, amount } => CommandKind::AwardXp {
            track: track.clone(),
            amount: *amount,
        },
        ActionDef::AddCurrency { currency, amount } => CommandKind::AddCurrency {
            currency: currency.clone(),
            amount: *amount,
        },
        ActionDef::SpendCurrency { currency, amount } => CommandKind::SpendCurrency {
            currency: currency.clone(),
            amount: *amount,
        },
        ActionDef::GrantItem { item, quantity } => CommandKind::GrantItem {
            item: item.clone(),
            quantity: *quantity,
        },
        ActionDef::GrantReward { reward } => CommandKind::GrantReward { reward: reward.clone() },
        ActionDef::UpdateProgress { challenge, amount } => CommandKind::ProgressChallenge {
            challenge: challenge.clone(),
            amount: *amount,
        },
        ActionDef::StartChallenge { challenge } => CommandKind::StartChallenge { challenge: challenge.clone() },
        ActionDef::CompleteChallenge { challenge } => CommandKind::CompleteChallenge { challenge: challenge.clone() },
        ActionDef::UnlockAchievement { achievement } => CommandKind::UnlockAchievement {
            achievement: achievement.clone(),
        },
        ActionDef::UpdateStreak { streak } => CommandKind::UpdateStreak {
            streak: streak.clone(),
            window_key: String::new(),
            grace_applied: false,
        },
        ActionDef::UpdateLeaderboard { leaderboard, score } => {
            // Score is a formula expression OR a literal number.
            let delta = match score.parse::<i64>() {
                Ok(v) => v,
                Err(_) => platform_formulas::evaluate_f64(score, ctx)
                    .map(|f| f.round() as i64)
                    .unwrap_or(0),
            };
            CommandKind::UpdateLeaderboard { leaderboard: leaderboard.clone(), delta, score: 0 }
        }
        ActionDef::GrantEntitlement { entitlement, duration_seconds } => CommandKind::GrantEntitlement {
            entitlement: entitlement.clone(),
            duration_seconds: *duration_seconds,
        },
        ActionDef::Notify { template, params } => CommandKind::Notify {
            template: template.clone(),
            params: params.clone(),
        },
        ActionDef::ShowPaywall { paywall } => CommandKind::ShowPaywall { paywall: paywall.clone() },
        ActionDef::StartWorkflow { workflow } => CommandKind::StartWorkflow {
            workflow: workflow.clone(),
            input: serde_json::Value::Null,
        },
        ActionDef::SetVariable { key, value } => CommandKind::SetVariable { key: key.clone(), value: value.clone() },
        ActionDef::EmitEvent { event_type, payload } => CommandKind::EmitEvent {
            event_type: event_type.clone(),
            payload: payload.clone(),
        },
        ActionDef::CallWebhook { endpoint } => CommandKind::CallWebhook { endpoint: endpoint.clone() },
    };
    let explanation = describe(&kind);
    Ok(Command::new(source, event_id, index, kind, explanation))
}

fn describe(k: &CommandKind) -> String {
    match k {
        CommandKind::AwardXp { track, amount } => format!("award {} xp on track `{}`", amount, track),
        CommandKind::AddCurrency { currency, amount } => format!("add {} `{}`", amount, currency),
        CommandKind::SpendCurrency { currency, amount } => format!("spend {} `{}`", amount, currency),
        CommandKind::GrantItem { item, quantity } => format!("grant {}x item `{}`", quantity, item),
        CommandKind::GrantReward { reward } => format!("grant reward `{}`", reward),
        CommandKind::ProgressChallenge { challenge, amount } => {
            format!("progress challenge `{}` by {}", challenge, amount)
        }
        CommandKind::StartChallenge { challenge } => format!("start challenge `{}`", challenge),
        CommandKind::CompleteChallenge { challenge } => format!("complete challenge `{}`", challenge),
        CommandKind::UnlockAchievement { achievement } => format!("unlock achievement `{}`", achievement),
        CommandKind::UpdateStreak { streak, window_key, grace_applied } => {
            if *grace_applied {
                format!("update streak `{}` (grace applied, window {})", streak, window_key)
            } else {
                format!("update streak `{}` (window {})", streak, window_key)
            }
        }
        CommandKind::UpdateLeaderboard { leaderboard, delta, .. } => {
            format!("update leaderboard `{}` by {}", leaderboard, delta)
        }
        CommandKind::GrantEntitlement { entitlement, duration_seconds } => {
            if *duration_seconds > 0 {
                format!("grant entitlement `{}` for {}s", entitlement, duration_seconds)
            } else {
                format!("grant entitlement `{}`", entitlement)
            }
        }
        CommandKind::Notify { template, .. } => format!("queue notification `{}`", template),
        CommandKind::ShowPaywall { paywall } => format!("show paywall `{}`", paywall),
        CommandKind::StartWorkflow { workflow, .. } => format!("start workflow `{}`", workflow),
        CommandKind::SetVariable { key, .. } => format!("set variable `{}`", key),
        CommandKind::EmitEvent { event_type, .. } => format!("emit event `{}`", event_type),
        CommandKind::CallWebhook { endpoint } => format!("call webhook `{}`", endpoint),
    }
}
