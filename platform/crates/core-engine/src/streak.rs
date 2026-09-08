//! Streak semantics (§24) — Time-Engine-driven cadence, grace, freeze, recovery.

use crate::state::{ActorState, StreakState};
use platform_common::config::StreakSpec;

/// Outcome of evaluating a streak for one event.
#[derive(Debug, Clone, PartialEq, serde::Serialize, serde::Deserialize)]
pub struct StreakOutcome {
    pub streak_id: String,
    pub before: i64,
    pub after: i64,
    pub best: i64,
    /// broken | extended | recovered | grace | frozen | no_action
    pub effect: String,
    pub window_key: String,
}

/// Compute how many cadence windows separate two window keys.
/// Returns 0 for identical keys, 1 for consecutive windows, 2+ for gaps.
fn window_distance(spec: &StreakSpec, key_a: &str, key_b: &str, instant: chrono::DateTime<chrono::Utc>, tz: &str) -> i64 {
    if key_a == key_b {
        return 0;
    }
    let cur = spec.window.resolve(instant, tz);
    let unit = (cur.ends_at - cur.starts_at).num_seconds().max(1);
    let (ea, eb) = match (key_epoch(key_a), key_epoch(key_b)) {
        (Some(a), Some(b)) => (a, b),
        // Unparseable keys: deterministic fallback = treat as consecutive.
        _ => return 1,
    };
    // eb (current) is at or after ea (last recorded).
    let delta = (eb - ea).max(0);
    match spec.window {
        // Fixed windows are aligned: consecutive keys differ by exactly one unit.
        platform_common::TimeWindow::Fixed { .. } => (delta / unit).max(1),
        // Rolling windows: any positive delta < unit still means "within one
        // window" -> consecutive; a delta of a full unit means one missed window.
        platform_common::TimeWindow::Rolling { .. } => 1 + delta / unit,
    }
}

fn key_epoch(key: &str) -> Option<i64> {
    let rest = key.split('/').nth(1)?;
    if key.starts_with("rolling/") {
        // rolling/{startTs}-{endTs}: the start timestamp identifies the window.
        return rest.split('-').next()?.parse::<i64>().ok();
    }
    // day/2026-09-08, week/2026-36, month/2026-09, year/2026, hour/2026-09-08T14 —
    // normalize by parsing the date portion and evaluating at UTC noon to stay
    // deterministic across timezones.
    let date_only = rest.split('T').next()?;
    let d = chrono::NaiveDate::parse_from_str(date_only, "%Y-%m-%d").ok()?;
    let dt = d
        .and_hms_opt(12, 0, 0)
        .unwrap_or_else(|| d.and_hms_opt(0, 0, 0).expect("midnight always constructible"));
    Some(dt.and_utc().timestamp())
}

/// Apply a qualifying action to a streak at `instant`.
pub fn apply_qualifying(
    spec: &StreakSpec,
    state: &mut ActorState,
    instant: chrono::DateTime<chrono::Utc>,
    tz: &str,
) -> StreakOutcome {
    let current_key = spec.window.resolve(instant, tz).key;
    let entry = state
        .streaks
        .entry(spec.id.clone())
        .or_insert_with(|| StreakState {
            streak_id: spec.id.clone(),
            current: 0,
            best: 0,
            last_window_key: String::new(),
            freezes_used: 0,
            active: false,
        });

    let before = entry.current;

    // First qualifying action ever.
    if entry.last_window_key.is_empty() {
        entry.current = 1;
        entry.best = entry.best.max(1);
        entry.last_window_key = current_key.clone();
        entry.active = true;
        return StreakOutcome {
            streak_id: spec.id.clone(),
            before,
            after: 1,
            best: entry.best,
            effect: "extended".into(),
            window_key: current_key,
        };
    }

    if entry.last_window_key == current_key {
        // Already counted this window.
        return StreakOutcome {
            streak_id: spec.id.clone(),
            before,
            after: entry.current,
            best: entry.best,
            effect: "no_action".into(),
            window_key: current_key,
        };
    }

    let gap = window_distance(spec, &entry.last_window_key, &current_key, instant, tz);
    let gap_windows = if gap <= 0 { 1 } else { gap };

    if gap_windows == 1 {
        // Consecutive window — extend.
        entry.current += 1;
        entry.best = entry.best.max(entry.current);
        entry.active = true;
        let effect = "extended".to_string();
        entry.last_window_key = current_key.clone();
        StreakOutcome {
            streak_id: spec.id.clone(),
            before,
            after: entry.current,
            best: entry.best,
            effect,
            window_key: current_key,
        }
    } else if gap_windows == 2 && spec.grace_seconds > 0 {
        // Missed exactly one window but within grace of the previous one:
        // treat as recovery (grace). Grace is bounded by elapsed seconds.
        let prev_end = prev_window_end(spec, &entry.last_window_key, instant, tz);
        let elapsed = instant.timestamp() - prev_end;
        if elapsed <= spec.grace_seconds {
            entry.current += 1;
            entry.best = entry.best.max(entry.current);
            entry.active = true;
            entry.last_window_key = current_key.clone();
            return StreakOutcome {
                streak_id: spec.id.clone(),
                before,
                after: entry.current,
                best: entry.best,
                effect: "grace".into(),
                window_key: current_key,
            };
        }
        reset(entry, &current_key);
        StreakOutcome {
            streak_id: spec.id.clone(),
            before,
            after: entry.current,
            best: entry.best,
            effect: "broken".into(),
            window_key: current_key,
        }
    } else {
        // Missed more than grace allows — reset (freeze consumes would be
        // applied by the control plane before calling this).
        reset(entry, &current_key);
        StreakOutcome {
            streak_id: spec.id.clone(),
            before,
            after: entry.current,
            best: entry.best,
            effect: "broken".into(),
            window_key: current_key,
        }
    }
}

fn reset(entry: &mut StreakState, key: &str) {
    entry.current = 1;
    entry.best = entry.best.max(1);
    entry.last_window_key = key.into();
    entry.active = true;
    entry.freezes_used = 0;
}

fn prev_window_end(spec: &StreakSpec, key: &str, instant: chrono::DateTime<chrono::Utc>, tz: &str) -> i64 {
    // Re-derive the end instant of the window identified by `key` using the
    // same deterministic key parser as `window_distance`.
    let cur = spec.window.resolve(instant, tz);
    let unit = (cur.ends_at - cur.starts_at).num_seconds().max(1);
    match key_epoch(key) {
        Some(e) => match spec.window {
            // Fixed keys are noon-anchored: the window ends half a unit later.
            platform_common::TimeWindow::Fixed { .. } => e + unit / 2,
            // Rolling keys are start-anchored: end is one unit after start.
            platform_common::TimeWindow::Rolling { .. } => e + unit,
        },
        None => cur.starts_at.timestamp() - unit,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use chrono::TimeZone;
    use platform_common::ObjectStatus;

    fn daily_spec(grace: i64) -> StreakSpec {
        StreakSpec {
            id: "s1".into(),
            name: "Daily".into(),
            event_type: "lesson.completed".into(),
            window: platform_common::TimeWindow::Fixed {
                unit: platform_common::CalendarUnit::Day,
                timezone: "UTC".into(),
            },
            grace_seconds: grace,
            freezes_allowed: 0,
            multiplier: None,
            status: ObjectStatus::Active,
        }
    }

    fn state() -> ActorState {
        ActorState::new("u1")
    }

    #[test]
    fn consecutive_days_extend() {
        let spec = daily_spec(0);
        let mut st = state();
        let d1 = chrono::Utc.with_ymd_and_hms(2026, 9, 6, 10, 0, 0).unwrap();
        let d2 = chrono::Utc.with_ymd_and_hms(2026, 9, 7, 10, 0, 0).unwrap();
        let d3 = chrono::Utc.with_ymd_and_hms(2026, 9, 8, 10, 0, 0).unwrap();
        let o1 = apply_qualifying(&spec, &mut st, d1, "UTC");
        assert_eq!(o1.after, 1);
        let o2 = apply_qualifying(&spec, &mut st, d2, "UTC");
        assert_eq!(o2.after, 2);
        assert_eq!(o2.effect, "extended");
        let o3 = apply_qualifying(&spec, &mut st, d3, "UTC");
        assert_eq!(o3.after, 3);
        assert_eq!(st.streaks["s1"].best, 3);
    }

    #[test]
    fn same_day_is_idempotent() {
        let spec = daily_spec(0);
        let mut st = state();
        let d = chrono::Utc.with_ymd_and_hms(2026, 9, 8, 9, 0, 0).unwrap();
        let d2 = chrono::Utc.with_ymd_and_hms(2026, 9, 8, 21, 0, 0).unwrap();
        apply_qualifying(&spec, &mut st, d, "UTC");
        let o = apply_qualifying(&spec, &mut st, d2, "UTC");
        assert_eq!(o.effect, "no_action");
        assert_eq!(o.after, 1);
    }

    #[test]
    fn gap_breaks_streak() {
        let spec = daily_spec(0);
        let mut st = state();
        let d1 = chrono::Utc.with_ymd_and_hms(2026, 9, 4, 10, 0, 0).unwrap();
        let d5 = chrono::Utc.with_ymd_and_hms(2026, 9, 8, 10, 0, 0).unwrap();
        apply_qualifying(&spec, &mut st, d1, "UTC");
        let o = apply_qualifying(&spec, &mut st, d5, "UTC");
        assert_eq!(o.effect, "broken");
        assert_eq!(o.after, 1);
        assert_eq!(o.best, 1);
    }

    #[test]
    fn one_day_gap_without_grace_breaks() {
        let spec = daily_spec(0);
        let mut st = state();
        let d6 = chrono::Utc.with_ymd_and_hms(2026, 9, 6, 10, 0, 0).unwrap();
        let d8 = chrono::Utc.with_ymd_and_hms(2026, 9, 8, 10, 0, 0).unwrap();
        apply_qualifying(&spec, &mut st, d6, "UTC");
        let o = apply_qualifying(&spec, &mut st, d8, "UTC");
        assert_eq!(o.effect, "broken");
    }
}
