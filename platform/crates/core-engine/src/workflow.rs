//! Workflow execution (§16): sequential steps with conditions, actions, delays.

use crate::commands::Command;
use crate::state::ActorState;
use platform_common::config::{ActionDef, WorkflowDef, WorkflowStepDef};
use platform_common::{Context, EngineError};
use std::collections::HashMap;

/// One workflow execution record (run history per §16).
#[derive(Debug, Clone, PartialEq, serde::Serialize, serde::Deserialize)]
pub struct WorkflowRun {
    pub execution_id: String,
    pub workflow_id: String,
    pub trigger_event_id: String,
    pub actor_id: String,
    pub status: WorkflowStatus,
    pub current_step: i64,
    pub steps_total: i64,
    #[serde(default)]
    pub commands: Vec<Command>,
    /// Delay continuation: step index to resume at, epoch ms when.
    #[serde(default)]
    pub resume_at: Option<(i64, i64)>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, serde::Serialize, serde::Deserialize, Default)]
#[serde(rename_all = "lowercase")]
pub enum WorkflowStatus {
    #[default]
    Running,
    Completed,
    Failed,
    Cancelled,
    Waiting,
}

/// Tick a workflow forward: executes steps until completion, failure, or a
/// delay boundary. Pure — returns the commands to apply + next state.
pub fn tick_workflow(
    def: &WorkflowDef,
    run: &mut WorkflowRun,
    state: &mut ActorState,
    ctx: &Context,
) -> Result<(), EngineError> {
    run.commands.clear();
    let steps: &[WorkflowStepDef] = &def.steps;
    if steps.is_empty() {
        run.status = WorkflowStatus::Completed;
        return Ok(());
    }

    let mut i = run.current_step.max(0) as usize;
    let mut guard = 0;
    while i < steps.len() {
        guard += 1;
        if guard > 10_000 {
            return Err(EngineError::transient("workflow step budget exceeded (possible loop)"));
        }
        run.current_step = i as i64;
        match &steps[i] {
            WorkflowStepDef::Condition { condition } => {
                let eval = platform_rules::evaluate_condition(condition, ctx);
                if eval.matched {
                    i += 1; // continue on the true branch (sequential model)
                } else {
                    // Sequential semantics: a failed condition halts the run
                    // (branching stays declarative via separate workflows).
                    run.status = WorkflowStatus::Completed;
                    return Ok(());
                }
            }
            WorkflowStepDef::Action { action } => {
                let cmd = crate::commands::expand_action(action, &format!("workflow:{}", def.id), &run.trigger_event_id, i, &ctx.values)?;
                run.commands.push(cmd);
                i += 1;
            }
            WorkflowStepDef::Delay { seconds } => {
                // Convert delay into a resume schedule; caller persists it.
                let now = chrono::Utc::now().timestamp_millis();
                run.resume_at = Some(((i + 1) as i64, now + (*seconds).max(0) * 1000));
                run.status = WorkflowStatus::Waiting;
                return Ok(());
            }
            WorkflowStepDef::CompleteChallenge { challenge } => {
                let cmd = Command::new(
                    &format!("workflow:{}", def.id),
                    &run.trigger_event_id,
                    i,
                    crate::commands::CommandKind::CompleteChallenge { challenge: challenge.clone() },
                    format!("workflow completes challenge `{}`", challenge),
                );
                run.commands.push(cmd);
                i += 1;
            }
        }
    }

    run.current_step = steps.len() as i64;
    run.status = WorkflowStatus::Completed;
    Ok(())
}

/// Create a new run for a workflow triggered by an event.
pub fn start_run(def: &WorkflowDef, trigger_event_id: &str, actor_id: &str) -> WorkflowRun {
    WorkflowRun {
        execution_id: platform_common::new_uuid(),
        workflow_id: def.id.clone(),
        trigger_event_id: trigger_event_id.into(),
        actor_id: actor_id.into(),
        status: WorkflowStatus::Running,
        current_step: 0,
        steps_total: def.steps.len() as i64,
        commands: Vec::new(),
        resume_at: None,
    }
}

/// Whether a workflow trigger matches the event.
pub fn trigger_matches(def: &WorkflowDef, event_type: &str) -> bool {
    use platform_common::config::WorkflowTrigger;
    match &def.trigger {
        WorkflowTrigger::Event { event_type: t } => t == event_type || t == "*",
        WorkflowTrigger::Manual => false,
        WorkflowTrigger::Schedule { .. } => false,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use platform_common::config::{ActionDef, WorkflowStepDef, WorkflowTrigger};
    use platform_common::{Condition, ObjectStatus, Operand};

    fn wf(steps: Vec<WorkflowStepDef>) -> WorkflowDef {
        WorkflowDef {
            id: "w1".into(),
            name: "Level-up flow".into(),
            trigger: WorkflowTrigger::Event { event_type: "lesson.completed".into() },
            steps,
            status: ObjectStatus::Active,
        }
    }

    fn ctx() -> Context {
        Context::default().set("user.level", serde_json::json!(1))
    }

    #[test]
    fn runs_to_completion_and_collects_commands() {
        let def = wf(vec![
            WorkflowStepDef::Condition {
                condition: Condition::Eq {
                    field: Operand::path("user.level"),
                    value: Operand::value(serde_json::json!(1)),
                },
            },
            WorkflowStepDef::Action {
                action: ActionDef::AwardXp { track: "default".into(), amount: 50 },
            },
            WorkflowStepDef::Action {
                action: ActionDef::Notify { template: "levelup".into(), params: Default::default() },
            },
        ]);
        let mut run = start_run(&def, "e1", "u1");
        let mut state = ActorState::new("u1");
        tick_workflow(&def, &mut run, &mut state, &ctx()).unwrap();
        assert_eq!(run.status, WorkflowStatus::Completed);
        assert_eq!(run.commands.len(), 2);
    }

    #[test]
    fn false_condition_completes_early() {
        let def = wf(vec![
            WorkflowStepDef::Condition {
                condition: Condition::Gt {
                    field: Operand::path("user.level"),
                    value: Operand::value(serde_json::json!(99)),
                },
            },
            WorkflowStepDef::Action {
                action: ActionDef::AwardXp { track: "default".into(), amount: 50 },
            },
        ]);
        let mut run = start_run(&def, "e1", "u1");
        let mut state = ActorState::new("u1");
        tick_workflow(&def, &mut run, &mut state, &ctx()).unwrap();
        assert_eq!(run.status, WorkflowStatus::Completed);
        assert!(run.commands.is_empty());
    }

    #[test]
    fn delay_pauses_workflow() {
        let def = wf(vec![
            WorkflowStepDef::Delay { seconds: 60 },
            WorkflowStepDef::Action {
                action: ActionDef::AwardXp { track: "default".into(), amount: 10 },
            },
        ]);
        let mut run = start_run(&def, "e1", "u1");
        let mut state = ActorState::new("u1");
        tick_workflow(&def, &mut run, &mut state, &ctx()).unwrap();
        assert_eq!(run.status, WorkflowStatus::Waiting);
        let (resume_step, at) = run.resume_at.unwrap();
        assert_eq!(resume_step, 1);
        assert!(at > chrono::Utc::now().timestamp_millis());
    }

    #[test]
    fn empty_workflow_completes() {
        let def = wf(vec![]);
        let mut run = start_run(&def, "e1", "u1");
        let mut state = ActorState::new("u1");
        tick_workflow(&def, &mut run, &mut state, &ctx()).unwrap();
        assert_eq!(run.status, WorkflowStatus::Completed);
    }

    #[test]
    fn trigger_matching() {
        let def = wf(vec![]);
        assert!(trigger_matches(&def, "lesson.completed"));
        assert!(!trigger_matches(&def, "other.event"));
    }

    use serde_json::json;
    use std::collections::HashMap;
    fn _unused(_: &HashMap<String, serde_json::Value>) {}
}
