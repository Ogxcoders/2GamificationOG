//! # platform-progression
//!
//! XP → level computation (§20): linear, exponential, and custom-formula models,
//! multiple independent tracks, and level-up event detection.
//!
//! Progression is pure math over the XP total — deterministic by construction.

pub use platform_common::config::{LevelTrack, ProgressionModel};

use platform_common::EngineError;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;

/// A user's position in one progression track.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct TrackState {
    pub track: String,
    pub xp: i64,
    pub level: i64,
    /// Level reached at this xp historically (for prestige analytics).
    #[serde(default)]
    pub highest_level: i64,
}

impl TrackState {
    pub fn new(track: impl Into<String>) -> Self {
        TrackState { track: track.into(), xp: 0, level: 0, highest_level: 0 }
    }
}

/// Compute the level for `xp` under the model. Deterministic, total.
pub fn level_for(model: &ProgressionModel, xp: i64) -> i64 {
    match model {
        ProgressionModel::Linear { xp_per_level } => {
            if *xp_per_level <= 0 {
                return 0;
            }
            xp.div_euclid(*xp_per_level)
        }
        ProgressionModel::Exponential { base, factor } => {
            if *base <= 0 || *factor <= 1.0 {
                // Degenerate config: fall back to linear with base step.
                if *base <= 0 {
                    return 0;
                }
                return xp.div_euclid(*base);
            }
            // Level n requires base * factor^n cumulative XP.
            // Solve iteratively (bounded — factor > 1 grows fast).
            let mut required: f64 = 0.0;
            let mut level: i64 = 0;
            let mut next: f64 = *base as f64;
            while (xp as f64) >= required + next && level < 1_000 {
                required += next;
                next *= *factor;
                level += 1;
            }
            level
        }
        ProgressionModel::Formula { expression } => formula_level(expression, xp),
    }
}

fn formula_level(expression: &str, xp: i64) -> i64 {
    let mut scope = HashMap::new();
    scope.insert("xp".to_string(), serde_json::json!(xp));
    match platform_formulas::evaluate_f64(expression, &scope) {
        Ok(v) => {
            if v.is_finite() && v >= 0.0 && v < 1e12 {
                v.floor() as i64
            } else {
                0
            }
        }
        Err(EngineError::Formula { .. }) => 0,
        Err(_) => 0,
    }
}

/// XP still required to reach the next level.
pub fn xp_to_next(model: &ProgressionModel, xp: i64) -> i64 {
    let next = level_for(model, xp) + 1;
    required_xp(model, next).saturating_sub(xp).max(0)
}

/// Cumulative XP required to *reach* `level` (0-based: level 0 at xp 0).
pub fn required_xp(model: &ProgressionModel, level: i64) -> i64 {
    if level <= 0 {
        return 0;
    }
    match model {
        ProgressionModel::Linear { xp_per_level } => (level.saturating_mul(*xp_per_level)).max(0),
        ProgressionModel::Exponential { base, factor } => {
            if *base <= 0 || *factor <= 1.0 {
                return if *base <= 0 { 0 } else { level.saturating_mul(*base) };
            }
            // sum_{k=0}^{level-1} base * factor^k
            let mut total: f64 = 0.0;
            let mut step: f64 = *base as f64;
            for _ in 0..level.min(1_000) {
                total += step;
                step *= *factor;
            }
            total.floor().max(0.0) as i64
        }
        ProgressionModel::Formula { expression } => {
            // Invert by search (formula models are monotonic in practice).
            let mut lo = 0i64;
            let mut hi = 1i64;
            while level_for_by_expr(expression, hi) < level && hi < 1 << 40 {
                hi *= 2;
            }
            while lo < hi {
                let mid = lo + (hi - lo) / 2;
                if level_for_by_expr(expression, mid) < level {
                    lo = mid + 1;
                } else {
                    hi = mid;
                }
            }
            lo
        }
    }
}

fn level_for_by_expr(expression: &str, xp: i64) -> i64 {
    let mut scope = HashMap::new();
    scope.insert("xp".to_string(), serde_json::json!(xp));
    platform_formulas::evaluate_f64(expression, &scope)
        .map(|v| if v.is_finite() && v >= 0.0 { v.floor() as i64 } else { 0 })
        .unwrap_or(0)
}

/// Result of applying an XP delta to a track.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct XpOutcome {
    pub track: String,
    pub xp_before: i64,
    pub xp_after: i64,
    pub level_before: i64,
    pub level_after: i64,
    pub leveled_up: bool,
    pub levels_gained: i64,
}

/// Apply an XP delta and detect level changes. XP never goes negative
/// (progression is monotonic by spec — remove-xp floors at 0).
pub fn apply_xp(model: &ProgressionModel, state: &mut TrackState, delta: i64) -> XpOutcome {
    let xp_before = state.xp;
    let level_before = state.level;
    state.xp = (state.xp + delta).max(0);
    state.level = level_for(model, state.xp);
    state.highest_level = state.highest_level.max(state.level);
    XpOutcome {
        track: state.track.clone(),
        xp_before,
        xp_after: state.xp,
        level_before,
        level_after: state.level,
        leveled_up: state.level > level_before,
        levels_gained: state.level - level_before,
    }
}

/// Milestone thresholds helper (§25): which thresholds did this xp cross?
pub fn milestones_crossed(thresholds: &[i64], before: i64, after: i64) -> Vec<i64> {
    thresholds
        .iter()
        .copied()
        .filter(|t| *t > before && *t <= after)
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn linear_levels() {
        let m = ProgressionModel::Linear { xp_per_level: 100 };
        assert_eq!(level_for(&m, 0), 0);
        assert_eq!(level_for(&m, 99), 0);
        assert_eq!(level_for(&m, 100), 1);
        assert_eq!(level_for(&m, 250), 2);
        assert_eq!(level_for(&m, -10), -1); // div_euclid floor semantics
        assert_eq!(required_xp(&m, 3), 300);
        assert_eq!(xp_to_next(&m, 250), 50);
    }

    #[test]
    fn exponential_levels() {
        let m = ProgressionModel::Exponential { base: 100, factor: 1.5 };
        assert_eq!(level_for(&m, 0), 0);
        assert_eq!(level_for(&m, 99), 0);
        assert_eq!(level_for(&m, 100), 1);
        // Level 2 at 100 + 150 = 250.
        assert_eq!(level_for(&m, 249), 1);
        assert_eq!(level_for(&m, 250), 2);
        assert_eq!(level_for(&m, 475), 3); // 100+150+225
        assert_eq!(required_xp(&m, 2), 250);
    }

    #[test]
    fn formula_model() {
        let m = ProgressionModel::Formula { expression: "floor(xp / 50)".into() };
        assert_eq!(level_for(&m, 0), 0);
        assert_eq!(level_for(&m, 49), 0);
        assert_eq!(level_for(&m, 50), 1);
        assert_eq!(level_for(&m, 175), 3);
        assert_eq!(required_xp(&m, 4), 200);
    }

    #[test]
    fn degenerate_configs_do_not_panic() {
        assert_eq!(level_for(&ProgressionModel::Linear { xp_per_level: 0 }, 500), 0);
        assert_eq!(level_for(&ProgressionModel::Linear { xp_per_level: -5 }, 500), 0);
        assert_eq!(level_for(&ProgressionModel::Exponential { base: 0, factor: 2.0 }, 500), 0);
        assert_eq!(level_for(&ProgressionModel::Exponential { base: 100, factor: 0.5 }, 500), 5);
        assert_eq!(level_for(&ProgressionModel::Formula { expression: "1/0".into() }, 500), 0);
    }

    #[test]
    fn apply_xp_and_levelup() {
        let m = ProgressionModel::Linear { xp_per_level: 100 };
        let mut st = TrackState::new("default");
        let out = apply_xp(&m, &mut st, 100);
        assert!(out.leveled_up);
        assert_eq!(out.level_after, 1);
        let out = apply_xp(&m, &mut st, 50);
        assert!(!out.leveled_up);
        assert_eq!(st.xp, 150);
        // Removal floors at zero.
        let out = apply_xp(&m, &mut st, -1_000);
        assert_eq!(st.xp, 0);
        assert_eq!(out.level_after, 0);
        assert_eq!(st.highest_level, 1);
    }

    #[test]
    fn milestones() {
        assert_eq!(milestones_crossed(&[10, 100, 1000], 5, 150), vec![10, 100]);
        assert!(milestones_crossed(&[10], 150, 5).is_empty());
    }
}
