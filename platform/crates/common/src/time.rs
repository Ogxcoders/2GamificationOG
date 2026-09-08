//! Time Engine (§18, §207).
//!
//! All core timestamps are UTC. Windows are resolved against an instant and a
//! timezone so "daily" is a *reusable temporal condition* — never hardcoded
//! inside streak/challenge features.

use chrono::{DateTime, Datelike, Duration, TimeZone, Timelike, Utc};
use serde::{Deserialize, Serialize};

/// Window definition attached to streaks, challenges, leaderboards, seasons.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum TimeWindow {
    /// Rolling window of N seconds ending at the evaluation instant.
    Rolling { seconds: i64 },
    /// Fixed calendar window anchored in a timezone.
    Fixed { unit: CalendarUnit, timezone: String },
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum CalendarUnit {
    Hour,
    Day,
    Week,
    Month,
    Year,
}

#[derive(Debug, Clone, PartialEq)]
pub struct ResolvedWindow {
    pub starts_at: DateTime<Utc>,
    pub ends_at: DateTime<Utc>,
    /// Stable identifier of the window instance, e.g. `day/2026-09-08` or `day/2026-W36-3`.
    pub key: String,
}

impl TimeWindow {
    /// Resolve the concrete window containing `instant` for a user in `tz`.
    /// Unknown timezone strings fall back to UTC (deterministic behavior).
    pub fn resolve(&self, instant: DateTime<Utc>, tz: &str) -> ResolvedWindow {
        resolve_window(self, instant, tz)
    }
}

fn tz_from_name(name: &str) -> chrono_tz::Tz {
    name.parse().unwrap_or(chrono_tz::UTC)
}

/// Resolve any supported window. Public helper used by streaks/leaderboards.
pub fn resolve_window(w: &TimeWindow, instant: DateTime<Utc>, tz: &str) -> ResolvedWindow {
    match w {
        TimeWindow::Rolling { seconds } => {
            let s = (*seconds).max(1);
            let ends = instant;
            let starts = ends - Duration::seconds(s);
            ResolvedWindow {
                starts_at: starts,
                ends_at: ends,
                key: format!("rolling/{}-{}", starts.timestamp(), ends.timestamp()),
            }
        }
        TimeWindow::Fixed { unit, timezone } => {
            let tzid = tz_from_name(timezone);
            let local = instant.with_timezone(&tzid);
            let (start_local, key) = match unit {
                CalendarUnit::Hour => {
                    let h = local
                        .with_minute(0)
                        .and_then(|d| d.with_second(0))
                        .and_then(|d| d.with_nanosecond(0))
                        .unwrap_or(local);
                    (h, format!("hour/{}", h.format("%Y-%m-%dT%H")))
                }
                CalendarUnit::Day => {
                    let d = local
                        .with_hour(0)
                        .and_then(|d| d.with_minute(0))
                        .and_then(|d| d.with_second(0))
                        .and_then(|d| d.with_nanosecond(0))
                        .unwrap_or(local);
                    (d, format!("day/{}", d.format("%Y-%m-%d")))
                }
                CalendarUnit::Week => {
                    // ISO week starting Monday.
                    let d = local
                        .with_hour(0)
                        .and_then(|d| d.with_minute(0))
                        .and_then(|d| d.with_second(0))
                        .and_then(|d| d.with_nanosecond(0))
                        .unwrap_or(local);
                    let dow = d.weekday().num_days_from_monday() as i64;
                    let start = d - Duration::days(dow);
                    (start, format!("week/{}", start.format("%Y-%W")))
                }
                CalendarUnit::Month => {
                    let d = local
                        .with_day(1)
                        .and_then(|d| d.with_hour(0))
                        .and_then(|d| d.with_minute(0))
                        .and_then(|d| d.with_second(0))
                        .and_then(|d| d.with_nanosecond(0))
                        .unwrap_or(local);
                    (d, format!("month/{}", d.format("%Y-%m")))
                }
                CalendarUnit::Year => {
                    let d = local
                        .with_month(1)
                        .and_then(|d| d.with_day(1))
                        .and_then(|d| d.with_hour(0))
                        .and_then(|d| d.with_minute(0))
                        .and_then(|d| d.with_second(0))
                        .and_then(|d| d.with_nanosecond(0))
                        .unwrap_or(local);
                    (d, format!("year/{}", d.format("%Y")))
                }
            };
            let starts_at = local_to_utc(tzid, &start_local.naive_local(), instant);
            let ends_local = next_boundary(start_local.naive_local(), *unit);
            let ends_at = local_to_utc(tzid, &ends_local, instant);
            ResolvedWindow {
                starts_at,
                ends_at,
                key,
            }
        }
    }
}

/// Deterministic, DST-safe local-to-UTC conversion.
///
/// * unambiguous -> the unique instant
/// * ambiguous (fall-back) -> earliest interpretation (window starts as early
///   as the wall clock allows; determinism matters more than politics)
/// * gap (spring-forward, local time never existed) -> step back until a
///   valid local time, convert, then step forward the same amount
fn local_to_utc(
    tz: chrono_tz::Tz,
    naive: &chrono::NaiveDateTime,
    fallback: DateTime<Utc>,
) -> DateTime<Utc> {
    use chrono::Offset;
    match tz.from_local_datetime(naive) {
        chrono::LocalResult::Single(dt) => dt.with_timezone(&Utc),
        chrono::LocalResult::Ambiguous(earliest, _) => earliest.with_timezone(&Utc),
        chrono::LocalResult::None => {
            for back in [1, 2] {
                let before = *naive - Duration::hours(back);
                match tz.from_local_datetime(&before) {
                    chrono::LocalResult::Single(dt) => {
                        return dt.with_timezone(&Utc) + Duration::hours(back)
                    }
                    chrono::LocalResult::Ambiguous(e, _) => {
                        return e.with_timezone(&Utc) + Duration::hours(back)
                    }
                    chrono::LocalResult::None => continue,
                }
            }
            fallback
        }
    }
}

fn next_boundary(local: chrono::NaiveDateTime, unit: CalendarUnit) -> chrono::NaiveDateTime {
    use chrono::Days;
    match unit {
        CalendarUnit::Hour => local + Duration::hours(1),
        CalendarUnit::Day => local + Days::new(1),
        CalendarUnit::Week => local + Days::new(7),
        CalendarUnit::Month => {
            let (y, m) = if local.month() == 12 {
                (local.year() + 1, 1)
            } else {
                (local.year(), local.month() + 1)
            };
            chrono::NaiveDate::from_ymd_opt(y, m, 1)
                .and_then(|d| d.and_hms_opt(0, 0, 0))
                .unwrap_or(local)
        }
        CalendarUnit::Year => chrono::NaiveDate::from_ymd_opt(local.year() + 1, 1, 1)
            .and_then(|d| d.and_hms_opt(0, 0, 0))
            .unwrap_or(local),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use chrono::TimeZone;

    #[test]
    fn rolling_window_bounds() {
        let w = TimeWindow::Rolling { seconds: 3600 };
        let instant = Utc.with_ymd_and_hms(2026, 9, 8, 12, 30, 0).unwrap();
        let r = w.resolve(instant, "UTC");
        assert_eq!(r.starts_at, instant - Duration::hours(1));
        assert_eq!(r.ends_at, instant);
    }

    #[test]
    fn daily_window_utc() {
        let w = TimeWindow::Fixed {
            unit: CalendarUnit::Day,
            timezone: "UTC".into(),
        };
        let instant = Utc.with_ymd_and_hms(2026, 9, 8, 15, 45, 10).unwrap();
        let r = w.resolve(instant, "UTC");
        assert_eq!(r.starts_at, Utc.with_ymd_and_hms(2026, 9, 8, 0, 0, 0).unwrap());
        assert_eq!(r.ends_at, Utc.with_ymd_and_hms(2026, 9, 9, 0, 0, 0).unwrap());
        assert_eq!(r.key, "day/2026-09-08");
    }

    #[test]
    fn daily_window_timezone_boundary() {
        // 2026-09-08 23:30 UTC == 2026-09-09 01:30 in Europe/Berlin (+2).
        let w = TimeWindow::Fixed {
            unit: CalendarUnit::Day,
            timezone: "Europe/Berlin".into(),
        };
        let instant = Utc.with_ymd_and_hms(2026, 9, 8, 23, 30, 0).unwrap();
        let r = w.resolve(instant, "Europe/Berlin");
        assert_eq!(r.key, "day/2026-09-09");
        assert_eq!(
            r.starts_at,
            Utc.with_ymd_and_hms(2026, 9, 8, 22, 0, 0).unwrap()
        );
    }

    #[test]
    fn week_window_starts_monday() {
        let w = TimeWindow::Fixed {
            unit: CalendarUnit::Week,
            timezone: "UTC".into(),
        };
        // 2026-09-08 is a Tuesday.
        let instant = Utc.with_ymd_and_hms(2026, 9, 8, 10, 0, 0).unwrap();
        let r = w.resolve(instant, "UTC");
        assert_eq!(r.starts_at, Utc.with_ymd_and_hms(2026, 9, 7, 0, 0, 0).unwrap());
    }

    #[test]
    fn month_and_year_keys() {
        let instant = Utc.with_ymd_and_hms(2026, 9, 8, 10, 0, 0).unwrap();
        let m = TimeWindow::Fixed {
            unit: CalendarUnit::Month,
            timezone: "UTC".into(),
        }
        .resolve(instant, "UTC");
        assert_eq!(m.key, "month/2026-09");
        let y = TimeWindow::Fixed {
            unit: CalendarUnit::Year,
            timezone: "UTC".into(),
        }
        .resolve(instant, "UTC");
        assert_eq!(y.key, "year/2026");
    }
}
