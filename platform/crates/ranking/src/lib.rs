//! # platform-ranking
//!
//! Deterministic leaderboard engine (§29, §206):
//! - Insert/refresh entries with stable tie-breaking (score + achieved_at +
//!   user id — the tie key NEVER includes entry_id, so ties share rank).
//! - Rank assignment by dense/competition semantics.
//! - Elo rating for head-to-head competition.
//! - Windowed boards via the Time Engine keys.

pub use platform_common::config::{Direction, LeaderboardSpec, TieBreaker};

use serde::{Deserialize, Serialize};

/// One leaderboard entry (runtime projection).
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Entry {
    pub user_id: String,
    pub score: i64,
    /// Epoch ms when the current score was achieved (tie-breaker input).
    pub achieved_at: i64,
}

/// A ranked row, produced by `rank`.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Ranked {
    pub rank: i64,
    pub user_id: String,
    pub score: i64,
    pub achieved_at: i64,
    /// Number of entries sharing this rank (ties).
    pub tied_with: i64,
}

/// Sort key implementing the deterministic tie-breaker policy (§206):
/// (score, achieved_at, user_id) — NEVER entry_id, so equal keys share rank.
fn sort_key(e: &Entry, spec: &LeaderboardSpec) -> (i64, i64, String) {
    let dir = match spec.direction {
        Direction::Highest => -e.score, // descending
        Direction::Lowest => e.score,   // ascending
    };
    let time_key = match spec.tie_breaker {
        TieBreaker::EarliestAchievedThenUserId => e.achieved_at,
        TieBreaker::LatestAchievedThenUserId => -e.achieved_at,
        TieBreaker::UserIdOnly => 0,
    };
    (dir, time_key, e.user_id.clone())
}

/// Rank a set of entries. Ties share the same rank (standard competition
/// ranking: 1, 2, 2, 4).
pub fn rank(entries: &[Entry], spec: &LeaderboardSpec) -> Vec<Ranked> {
    let mut sorted: Vec<&Entry> = entries.iter().collect();
    sorted.sort_by_key(|e| sort_key(e, spec));

    let mut out: Vec<Ranked> = Vec::with_capacity(sorted.len());
    let mut current_rank: i64 = 0;
    let mut tie_count: i64 = 0;
    let mut prev_key: Option<(i64, i64, String)> = None;

    for e in sorted {
        let key = sort_key(e, spec);
        let is_tie = match &prev_key {
            Some(p) => {
                // Same score + same tie-break time component => tie.
                p.0 == key.0 && p.1 == key.1
            }
            None => false,
        };
        if !is_tie || prev_key.is_none() {
            current_rank = (out.len() as i64) + 1;
            tie_count = 1;
        } else {
            tie_count += 1;
        }
        out.push(Ranked {
            rank: current_rank,
            user_id: e.user_id.clone(),
            score: e.score,
            achieved_at: e.achieved_at,
            tied_with: 0, // filled below
        });
        prev_key = Some(key);
    }

    // Second pass: fill tied_with counts.
    let mut i = 0;
    while i < out.len() {
        let r = out[i].rank;
        let mut j = i;
        let mut count = 0;
        while j < out.len() && out[j].rank == r {
            count += 1;
            j += 1;
        }
        for row in out.iter_mut().take(j).skip(i) {
            row.tied_with = count;
        }
        i = j;
    }
    out
}

/// A user's position, including neighborhood for UI context.
pub fn position_of(
    entries: &[Entry],
    spec: &LeaderboardSpec,
    user_id: &str,
) -> Option<Ranked> {
    rank(entries, spec).into_iter().find(|r| r.user_id == user_id)
}

/// Top-N slice of the ranked board.
pub fn top_n(entries: &[Entry], spec: &LeaderboardSpec, n: usize) -> Vec<Ranked> {
    rank(entries, spec).into_iter().take(n).collect()
}

// ─────────────────────────────────────────────────────────────────────────────
// Elo (§30 competition)
// ─────────────────────────────────────────────────────────────────────────────

/// Standard Elo expected score.
pub fn elo_expected(rating_a: f64, rating_b: f64) -> f64 {
    1.0 / (1.0 + 10f64.powf((rating_b - rating_a) / 400.0))
}

/// Elo update for one player. Deterministic given the inputs.
/// Returns the new rating.
pub fn elo_update(rating: f64, opponent: f64, score: f64, k: f64) -> f64 {
    let expected = elo_expected(rating, opponent);
    (rating + k * (score - expected)).max(0.0)
}

#[cfg(test)]
mod tests {
    use super::*;
    use platform_common::ObjectStatus;

    fn spec(direction: Direction, tb: TieBreaker) -> LeaderboardSpec {
        LeaderboardSpec {
            id: "lb1".into(),
            name: "Test".into(),
            direction,
            tie_breaker: tb,
            metric: platform_common::config::LeaderboardMetric::Xp,
            track: "default".into(),
            window: None,
            status: ObjectStatus::Active,
        }
    }

    fn entry(u: &str, s: i64, at: i64) -> Entry {
        Entry { user_id: u.into(), score: s, achieved_at: at }
    }

    #[test]
    fn ties_share_rank_excluding_entry_id() {
        let spec = spec(Direction::Highest, TieBreaker::EarliestAchievedThenUserId);
        let entries = vec![
            entry("u1", 100, 10),
            entry("u2", 100, 10), // tie with u1
            entry("u3", 100, 20), // later achievement -> rank 3
            entry("u4", 90, 5),
        ];
        let ranked = rank(&entries, &spec);
        assert_eq!(ranked[0].rank, 1);
        assert_eq!(ranked[1].rank, 1);
        assert_eq!(ranked[1].user_id, "u2");
        assert_eq!(ranked[2].rank, 3);
        assert_eq!(ranked[3].rank, 4);
        assert_eq!(ranked[0].tied_with, 2);
    }

    #[test]
    fn ascending_direction() {
        let spec = spec(Direction::Lowest, TieBreaker::EarliestAchievedThenUserId);
        let entries = vec![entry("u1", 50, 1), entry("u2", 10, 1), entry("u3", 10, 2)];
        let ranked = rank(&entries, &spec);
        assert_eq!(ranked[0].score, 10);
        assert_eq!(ranked[1].score, 10);
        assert_eq!(ranked[2].score, 50);
        assert_eq!(ranked[0].user_id, "u2"); // earlier achieved_at wins the tie
    }

    #[test]
    fn latest_tie_breaker_prefers_recent() {
        let spec = spec(Direction::Highest, TieBreaker::LatestAchievedThenUserId);
        let entries = vec![entry("u1", 100, 10), entry("u2", 100, 20)];
        let ranked = rank(&entries, &spec);
        assert_eq!(ranked[0].user_id, "u2");
    }

    #[test]
    fn position_and_topn() {
        let spec = spec(Direction::Highest, TieBreaker::EarliestAchievedThenUserId);
        let entries = vec![
            entry("u1", 100, 1),
            entry("u2", 90, 1),
            entry("u3", 80, 1),
            entry("u4", 70, 1),
        ];
        assert_eq!(position_of(&entries, &spec, "u3").unwrap().rank, 3);
        assert_eq!(top_n(&entries, &spec, 2).len(), 2);
        assert_eq!(position_of(&entries, &spec, "ghost"), None);
    }

    #[test]
    fn empty_board_is_empty() {
        let spec = spec(Direction::Highest, TieBreaker::EarliestAchievedThenUserId);
        assert!(rank(&[], &spec).is_empty());
    }

    #[test]
    fn elo_math() {
        // 1500 vs 1500 -> expected 0.5
        assert!((elo_expected(1500.0, 1500.0) - 0.5).abs() < 1e-9);
        // 1600 beats 1400 with k=32: new = 1600 + 32*(1-0.76) = 1607.688
        let new = elo_update(1600.0, 1400.0, 1.0, 32.0);
        assert!((new - 1607.688).abs() < 0.01, "got {}", new);
        // Loss floors are natural (no negatives for sane inputs).
        let floor = elo_update(10.0, 2000.0, 0.0, 32.0);
        assert!(floor >= 0.0);
    }
}
