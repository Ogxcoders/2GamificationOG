//! # platform-simulation
//!
//! Deterministic simulation engine (§71, §231, §232 side-effect safety):
//! - Seeded PRNG (SplitMix64) — same seed + same config + same inputs =
//!   identical results, always.
//! - Synthetic cohorts: generate N users with behavior distributions.
//! - Simulation is pure — NO external side effects by design (the engine
//!   processes a shadow event stream; the control plane decides what to do
//!   with the outcome).
//! - Reports: outcome distributions, state diffs, performance metrics.

use serde::{Deserialize, Serialize};

/// SplitMix64 — tiny, fast, deterministic, good statistical quality.
pub struct Prng {
    state: u64,
}

impl Prng {
    pub fn new(seed: u64) -> Self {
        Prng { state: seed }
    }

    pub fn next_u64(&mut self) -> u64 {
        self.state = self.state.wrapping_add(0x9E37_79B9_7F4A_7C15);
        let mut z = self.state;
        z = (z ^ (z >> 30)).wrapping_mul(0xBF58_476D_1CE4_E5B9);
        z = (z ^ (z >> 27)).wrapping_mul(0x94D0_49BB_1331_11EB);
        z ^ (z >> 31)
    }

    /// Uniform float in [0, 1).
    pub fn next_f64(&mut self) -> f64 {
        (self.next_u64() >> 11) as f64 / (1u64 << 53) as f64
    }

    /// Uniform integer in [lo, hi] inclusive.
    pub fn next_range(&mut self, lo: i64, hi: i64) -> i64 {
        if hi <= lo {
            return lo;
        }
        lo + (self.next_u64() % ((hi - lo + 1) as u64)) as i64
    }

    /// Bernoulli draw.
    pub fn chance(&mut self, p: f64) -> bool {
        self.next_f64() < p.clamp(0.0, 1.0)
    }

    /// Pick from a weighted distribution.
    pub fn weighted(&mut self, weights: &[f64]) -> usize {
        let total: f64 = weights.iter().sum();
        if total <= 0.0 || weights.is_empty() {
            return 0;
        }
        let mut r = self.next_f64() * total;
        for (i, w) in weights.iter().enumerate() {
            r -= w;
            if r <= 0.0 {
                return i;
            }
        }
        weights.len() - 1
    }
}

/// One simulated user's behavior profile.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct SimUser {
    pub user_id: String,
    /// Mean events per active day.
    pub activity: f64,
    /// Engagement segment weight.
    pub engagement: usize,
}

/// Event template: event type + payload generator inputs.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct EventTemplate {
    pub event_type: String,
    /// Min/max payload.points range.
    #[serde(default)]
    pub points_range: (i64, i64),
    /// Relative frequency weight.
    #[serde(default = "default_weight")]
    pub weight: f64,
}

fn default_weight() -> f64 {
    1.0
}

/// Simulation request.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct SimulationRequest {
    pub project_id: String,
    pub environment_id: String,
    pub config_version: i64,
    /// Determinism: identical seeds reproduce identical simulations.
    pub seed: u64,
    pub users: usize,
    pub days: usize,
    #[serde(default, deserialize_with = "platform_common::null_vec::deserialize")]
    pub templates: Vec<EventTemplate>,
}

/// Simulation outcome summary.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct SimulationReport {
    pub seed: u64,
    pub users: usize,
    pub days: usize,
    pub total_events: usize,
    pub total_actions: usize,
    /// Level -> user count distribution.
    pub level_distribution: std::collections::BTreeMap<i64, usize>,
    /// Currency -> total earned.
    pub currency_totals: std::collections::BTreeMap<String, i64>,
    /// Compute time in microseconds (performance metric, §71).
    pub compute_micros: u128,
}

/// Generated synthetic event stream (deterministic).
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct SimEvent {
    pub day: usize,
    pub user_id: String,
    pub event_type: String,
    pub points: i64,
}

/// Generate the deterministic event stream for the cohort.
pub fn generate_events(req: &SimulationRequest) -> Vec<SimEvent> {
    let mut rng = Prng::new(req.seed);
    let mut users = Vec::with_capacity(req.users);
    for i in 0..req.users {
        users.push(SimUser {
            user_id: format!("sim_{}", i + 1),
            // Activity follows a power-law-ish skew: many low, few high.
            activity: 0.2 + 8.0 * (rng.next_f64().powi(2)),
            engagement: rng.weighted(&[60.0, 25.0, 10.0, 5.0]),
        });
    }

    let templates = if req.templates.is_empty() {
        default_templates()
    } else {
        req.templates.clone()
    };

    let mut events = Vec::new();
    for day in 0..req.days {
        for u in &users {
            // Active-day probability driven by engagement segment.
            let active_p = [0.3, 0.5, 0.75, 0.95][u.engagement.min(3)];
            if !rng.chance(active_p) {
                continue;
            }
            let n = (rng.next_f64() * u.activity).round().max(1.0) as usize;
            for _ in 0..n {
                let weights: Vec<f64> = templates.iter().map(|t| t.weight).collect();
                let t = &templates[rng.weighted(&weights)];
                events.push(SimEvent {
                    day,
                    user_id: u.user_id.clone(),
                    event_type: t.event_type.clone(),
                    points: rng.next_range(t.points_range.0, t.points_range.1),
                });
            }
        }
    }
    events
}

fn default_templates() -> Vec<EventTemplate> {
    vec![
        EventTemplate { event_type: "lesson.completed".into(), points_range: (10, 50), weight: 5.0 },
        EventTemplate { event_type: "quiz.passed".into(), points_range: (20, 80), weight: 2.0 },
        EventTemplate { event_type: "workout.finished".into(), points_range: (15, 40), weight: 1.0 },
    ]
}

#[cfg(test)]
mod tests {
    use super::*;

    fn req(seed: u64, users: usize, days: usize) -> SimulationRequest {
        SimulationRequest {
            project_id: "p1".into(),
            environment_id: "development".into(),
            config_version: 1,
            seed,
            users,
            days,
            templates: vec![],
        }
    }

    #[test]
    fn prng_is_deterministic() {
        let mut a = Prng::new(42);
        let mut b = Prng::new(42);
        for _ in 0..100 {
            assert_eq!(a.next_u64(), b.next_u64());
        }
        let mut c = Prng::new(43);
        assert_ne!(a.next_u64(), c.next_u64());
    }

    #[test]
    fn range_bounds() {
        let mut rng = Prng::new(1);
        for _ in 0..1_000 {
            let v = rng.next_range(3, 7);
            assert!((3..=7).contains(&v));
        }
        assert_eq!(rng.next_range(5, 5), 5);
        assert_eq!(rng.next_range(9, 2), 9); // degenerate -> lo
    }

    #[test]
    fn weighted_picks_valid_index() {
        let mut rng = Prng::new(7);
        let idx = rng.weighted(&[1.0, 3.0, 1.0]);
        assert!(idx < 3);
        assert_eq!(rng.weighted(&[]), 0);
        assert_eq!(rng.weighted(&[0.0, 0.0]), 0);
    }

    #[test]
    fn same_seed_same_stream() {
        let e1 = generate_events(&req(99, 50, 7));
        let e2 = generate_events(&req(99, 50, 7));
        assert_eq!(e1, e2);
        assert!(!e1.is_empty());
    }

    #[test]
    fn different_seed_different_stream() {
        let e1 = generate_events(&req(99, 50, 7));
        let e2 = generate_events(&req(100, 50, 7));
        assert_ne!(e1, e2);
    }

    #[test]
    fn events_reference_valid_users_and_days() {
        let r = req(5, 20, 3);
        let events = generate_events(&r);
        for e in &events {
            assert!(e.user_id.starts_with("sim_"));
            assert!(e.day < 3);
        }
    }
}
