//! # platform-economy
//!
//! Wallet / ledger kernel (§27, §204, §121, §15 transactional atomicity):
//!
//! - **Never mutate balances directly** — every change is an append-only
//!   ledger entry; balances are projections (`Wallet = sum(ledger)`).
//! - Reversals are compensating entries carrying the opposite signed effect
//!   (financial history is never rewritten).
//! - Spend validation checks the **NET final balance** across the whole
//!   pending batch (two-pass), not entry-by-entry — the atomicity lesson.
//! - Caps, negative-balance policy, and replay-safe grant identities
//!   (reward_source + source_id + recipient) per §205.

pub use platform_common::config::CurrencyDef;

use platform_common::EngineError;
use serde::{Deserialize, Serialize};

/// One immutable ledger line.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct LedgerEntry {
    pub entry_id: String,
    pub wallet_id: String,
    pub currency: String,
    /// Signed effect on the balance (+ earn, - spend).
    pub amount: i64,
    /// Why: rule_id / purchase_id / admin_action...
    pub source: String,
    pub reason: String,
    /// External/reference identity for dedup & audit.
    pub correlation_id: String,
    pub causation_id: String,
    /// Idempotency: (source, reference) must be unique — replays are rejected.
    pub reference: String,
    pub occurred_at: i64,
    /// Reversal linkage: original entry id when this line compensates.
    #[serde(default)]
    pub reverses: Option<String>,
}

impl LedgerEntry {
    pub fn earn(wallet_id: &str, currency: &str, amount: i64, source: &str, reference: &str, now_ms: i64) -> Self {
        LedgerEntry {
            entry_id: platform_common::new_uuid(),
            wallet_id: wallet_id.into(),
            currency: currency.into(),
            amount: amount.abs(),
            source: source.into(),
            reason: "earn".into(),
            correlation_id: platform_common::new_uuid(),
            causation_id: String::new(),
            reference: reference.into(),
            occurred_at: now_ms,
            reverses: None,
        }
    }

    pub fn spend(wallet_id: &str, currency: &str, amount: i64, source: &str, reference: &str, now_ms: i64) -> Self {
        LedgerEntry {
            entry_id: platform_common::new_uuid(),
            wallet_id: wallet_id.into(),
            currency: currency.into(),
            amount: -amount.abs(),
            source: source.into(),
            reason: "spend".into(),
            correlation_id: platform_common::new_uuid(),
            causation_id: String::new(),
            reference: reference.into(),
            occurred_at: now_ms,
            reverses: None,
        }
    }
}

/// In-memory ledger kernel; the Go control plane mirrors these semantics in
/// SQL transactions (same invariants, same validation order).
#[derive(Debug, Clone, Default)]
pub struct Ledger {
    pub entries: Vec<LedgerEntry>,
    /// (source, reference) -> entry_id for idempotency.
    seen: std::collections::HashSet<(String, String)>,
}

impl Ledger {
    pub fn new() -> Self {
        Ledger::default()
    }

    /// Projected balance of a wallet/currency = sum of signed amounts.
    pub fn balance(&self, wallet_id: &str, currency: &str) -> i64 {
        self.entries
            .iter()
            .filter(|e| e.wallet_id == wallet_id && e.currency == currency)
            .map(|e| e.amount)
            .sum()
    }

    /// Wallet projection: currency -> balance.
    pub fn wallet(&self, wallet_id: &str) -> std::collections::BTreeMap<String, i64> {
        let mut m = std::collections::BTreeMap::new();
        for e in &self.entries {
            if e.wallet_id == wallet_id {
                *m.entry(e.currency.clone()).or_insert(0i64) += e.amount;
            }
        }
        m
    }

    /// Append entries with full invariant validation (§15 + §121):
    /// 1. reference dedup (replay protection)
    /// 2. currency existence + policy (negative, cap)
    /// 3. **NET final balance** across the batch (two-pass)
    pub fn apply(&mut self, batch: &[LedgerEntry], currencies: &[CurrencyDef]) -> Result<(), EngineError> {
        if batch.is_empty() {
            return Ok(());
        }

        // Pass 1: idempotency.
        for e in batch {
            let key = (e.source.clone(), e.reference.clone());
            if self.seen.contains(&key) {
                return Err(EngineError::Economy {
                    message: format!(
                        "duplicate ledger reference `{}` from source `{}` — replay rejected",
                        e.reference, e.source
                    ),
                    balance_after: None,
                });
            }
        }

        // Pass 2: NET final balances.
        let mut net: std::collections::HashMap<(String, String), i64> = std::collections::HashMap::new();
        for e in batch {
            *net
                .entry((e.wallet_id.clone(), e.currency.clone()))
                .or_insert(0i64) += e.amount;
        }

        for ((wallet_id, currency), delta) in &net {
            let final_balance = self.balance(wallet_id, currency) + delta;
            let def = currencies
                .iter()
                .find(|c| c.id == *currency)
                .ok_or_else(|| EngineError::ReferenceNotFound {
                    reference: currency.clone(),
                    hint: format!(
                        "wallet `{}` references a currency that does not exist in this environment — add currency `{}` or change the reward configuration",
                        wallet_id, currency
                    ),
                })?;

            if final_balance < 0 && !def.allow_negative {
                return Err(EngineError::Economy {
                    message: format!(
                        "wallet `{}` would go negative for currency `{}` ({} -> {}) — negative balances are disabled for this currency",
                        wallet_id, currency, final_balance - delta, final_balance
                    ),
                    balance_after: Some(final_balance),
                });
            }
            if def.cap > 0 && final_balance > def.cap {
                return Err(EngineError::Economy {
                    message: format!(
                        "wallet `{}` would exceed the cap for currency `{}` ({} > {}) — reduce the award or raise the cap",
                        wallet_id, currency, final_balance, def.cap
                    ),
                    balance_after: Some(final_balance),
                });
            }
        }

        // Commit.
        for e in batch {
            self.seen.insert((e.source.clone(), e.reference.clone()));
            self.entries.push(e.clone());
        }
        Ok(())
    }

    /// Reversal: append compensating entries with opposite signed effect.
    /// Financial history is never rewritten (§204).
    pub fn reverse(&mut self, entry_id: &str, reason: &str, now_ms: i64, currencies: &[CurrencyDef]) -> Result<LedgerEntry, EngineError> {
        let original = self
            .entries
            .iter()
            .find(|e| e.entry_id == entry_id)
            .ok_or_else(|| EngineError::ReferenceNotFound {
                reference: entry_id.into(),
                hint: "cannot reverse an entry that does not exist in this environment".into(),
            })?;

        // Double-reversal guard.
        if self.entries.iter().any(|e| e.reverses.as_deref() == Some(entry_id)) {
            return Err(EngineError::Economy {
                message: format!("entry `{}` is already reversed — double reversal rejected", entry_id),
                balance_after: None,
            });
        }

        let mut comp = original.clone();
        comp.entry_id = platform_common::new_uuid();
        comp.amount = -original.amount; // opposite signed effect
        comp.reason = format!("reversal: {}", reason);
        comp.reference = format!("reversal:{}", entry_id);
        comp.source = "system.reversal".into();
        comp.occurred_at = now_ms;
        comp.reverses = Some(entry_id.into());
        comp.correlation_id = original.correlation_id.clone();
        comp.causation_id = original.entry_id.clone();

        // A reversal restoring balance can never go negative (it undoes a prior
        // legal state) — but still validate for cap edge cases.
        self.apply(&[comp.clone()], currencies)?;
        Ok(comp)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use platform_common::ObjectStatus;

    fn coin(cap: i64, allow_negative: bool) -> CurrencyDef {
        CurrencyDef {
            id: "coin".into(),
            name: "Coins".into(),
            cap,
            allow_negative,
            status: ObjectStatus::Active,
        }
    }

    fn now() -> i64 {
        1_772_000_000_000
    }

    #[test]
    fn earn_spend_and_balance_projection() {
        let mut l = Ledger::new();
        let cs = vec![coin(0, false)];
        l.apply(
            &[LedgerEntry::earn("w1", "coin", 100, "rule.r1", "e1", now())],
            &cs,
        )
        .unwrap();
        l.apply(
            &[LedgerEntry::spend("w1", "coin", 30, "shop.purchase", "e2", now())],
            &cs,
        )
        .unwrap();
        assert_eq!(l.balance("w1", "coin"), 70);
        assert_eq!(l.entries.len(), 2);
    }

    #[test]
    fn replay_is_rejected() {
        let mut l = Ledger::new();
        let cs = vec![coin(0, false)];
        let e = LedgerEntry::earn("w1", "coin", 50, "rule.r1", "evt-1", now());
        l.apply(&[e.clone()], &cs).unwrap();
        let err = l.apply(&[e], &cs).unwrap_err();
        assert!(matches!(err, EngineError::Economy { .. }));
        assert_eq!(l.balance("w1", "coin"), 50);
    }

    #[test]
    fn spend_validates_net_balance_not_entrywise() {
        // Two earns in one batch + one spend — the batch is valid NET.
        let mut l = Ledger::new();
        let cs = vec![coin(0, false)];
        let batch = vec![
            LedgerEntry::earn("w1", "coin", 60, "rule.r1", "a", now()),
            LedgerEntry::earn("w1", "coin", 40, "rule.r2", "b", now()),
            LedgerEntry::spend("w1", "coin", 100, "shop", "c", now()),
        ];
        l.apply(&batch, &cs).unwrap();
        assert_eq!(l.balance("w1", "coin"), 0);
    }

    #[test]
    fn negative_balance_rejected() {
        let mut l = Ledger::new();
        let cs = vec![coin(0, false)];
        let err = l
            .apply(&[LedgerEntry::spend("w1", "coin", 10, "shop", "x", now())], &cs)
            .unwrap_err();
        assert!(matches!(err, EngineError::Economy { .. }));
    }

    #[test]
    fn negative_allowed_when_configured() {
        let mut l = Ledger::new();
        let cs = vec![coin(0, true)];
        l.apply(&[LedgerEntry::spend("w1", "coin", 10, "shop", "x", now())], &cs)
            .unwrap();
        assert_eq!(l.balance("w1", "coin"), -10);
    }

    #[test]
    fn cap_enforced_on_net() {
        let mut l = Ledger::new();
        let cs = vec![coin(100, false)];
        l.apply(&[LedgerEntry::earn("w1", "coin", 90, "r", "1", now())], &cs)
            .unwrap();
        let err = l
            .apply(&[LedgerEntry::earn("w1", "coin", 20, "r", "2", now())], &cs)
            .unwrap_err();
        assert!(matches!(err, EngineError::Economy { .. }));
    }

    #[test]
    fn unknown_currency_is_actionable_reference_error() {
        let mut l = Ledger::new();
        let err = l
            .apply(&[LedgerEntry::earn("w1", "gem", 10, "r", "1", now())], &[])
            .unwrap_err();
        match err {
            EngineError::ReferenceNotFound { hint, .. } => {
                assert!(hint.contains("add currency"), "hint must be actionable: {}", hint);
            }
            other => panic!("expected ReferenceNotFound, got {:?}", other),
        }
    }

    #[test]
    fn reversal_carries_opposite_sign_and_blocks_double_reversal() {
        let mut l = Ledger::new();
        let cs = vec![coin(0, false)];
        l.apply(&[LedgerEntry::earn("w1", "coin", 100, "rule.r1", "e1", now())], &cs)
            .unwrap();
        let original_id = l.entries[0].entry_id.clone();

        let comp = l.reverse(&original_id, "fraud correction", now(), &cs).unwrap();
        assert_eq!(comp.amount, -100);
        assert_eq!(comp.reverses.as_deref(), Some(original_id.as_str()));
        assert_eq!(l.balance("w1", "coin"), 0);

        let err = l.reverse(&original_id, "again", now(), &cs).unwrap_err();
        assert!(matches!(err, EngineError::Economy { .. }));
        assert_eq!(l.entries.len(), 2);
    }

    #[test]
    fn reversal_of_spend_restores_funds() {
        let mut l = Ledger::new();
        let cs = vec![coin(0, false)];
        l.apply(&[LedgerEntry::earn("w1", "coin", 50, "r", "1", now())], &cs)
            .unwrap();
        l.apply(&[LedgerEntry::spend("w1", "coin", 50, "shop", "2", now())], &cs)
            .unwrap();
        let spend_id = l.entries[1].entry_id.clone();
        l.reverse(&spend_id, "refund", now(), &cs).unwrap();
        assert_eq!(l.balance("w1", "coin"), 50);
    }

    #[test]
    fn wallet_projection_multi_currency() {
        let mut l = Ledger::new();
        let cs = vec![coin(0, false), CurrencyDef {
            id: "gem".into(),
            name: "Gems".into(),
            cap: 0,
            allow_negative: false,
            status: ObjectStatus::Active,
        }];
        l.apply(
            &[
                LedgerEntry::earn("w1", "coin", 10, "r", "1", now()),
                LedgerEntry::earn("w1", "gem", 3, "r", "2", now()),
            ],
            &cs,
        )
        .unwrap();
        let w = l.wallet("w1");
        assert_eq!(w.get("coin"), Some(&10));
        assert_eq!(w.get("gem"), Some(&3));
    }
}
