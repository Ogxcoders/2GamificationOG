//! Condition tree evaluation against the context scope.

use platform_common::{resolve_path, Condition, Context, Operand};
use serde_json::Value;

/// Result of one rule/condition evaluation, for decision traces.
#[derive(Debug, Clone, PartialEq)]
pub struct RuleEvaluation {
    pub matched: bool,
    /// Populated for failed checks: human-readable reason (§106).
    pub reason: String,
}

/// Evaluate a condition tree. Always returns a boolean — malformed references
/// are `false` with a reason rather than errors (rule-skip semantics).
pub fn evaluate_condition(cond: &Condition, ctx: &Context) -> RuleEvaluation {
    match cond {
        Condition::And { all } => {
            for c in all {
                let r = evaluate_condition(c, ctx);
                if !r.matched {
                    return RuleEvaluation { matched: false, reason: format!("and: {}", r.reason) };
                }
            }
            RuleEvaluation { matched: true, reason: String::new() }
        }
        Condition::Or { any } => {
            if any.is_empty() {
                return RuleEvaluation { matched: false, reason: "or: no sub-conditions".into() };
            }
            let mut why = Vec::new();
            for c in any {
                let r = evaluate_condition(c, ctx);
                if r.matched {
                    return RuleEvaluation { matched: true, reason: String::new() };
                }
                why.push(r.reason);
            }
            RuleEvaluation { matched: false, reason: format!("or: [{}]", why.join("; ")) }
        }
        Condition::Not { condition } => {
            let inner = evaluate_condition(condition, ctx);
            RuleEvaluation {
                matched: !inner.matched,
                reason: if inner.matched { "not: inner matched".into() } else { String::new() },
            }
        }
        Condition::Eq { field, value } => cmp_cond(field, value, ctx, |o| o == 0, "eq"),
        Condition::Neq { field, value } => cmp_cond(field, value, ctx, |o| o != 0, "neq"),
        Condition::Gt { field, value } => cmp_cond(field, value, ctx, |o| o > 0, "gt"),
        Condition::Gte { field, value } => cmp_cond(field, value, ctx, |o| o >= 0, "gte"),
        Condition::Lt { field, value } => cmp_cond(field, value, ctx, |o| o < 0, "lt"),
        Condition::Lte { field, value } => cmp_cond(field, value, ctx, |o| o <= 0, "lte"),
        Condition::In { field, values } => {
            let v = as_value(resolve_field(field, ctx));
            let m = values.iter().any(|x| values_eq(&v, x));
            RuleEvaluation { matched: m, reason: if m { String::new() } else { format!("in: {:?} not in list", v) } }
        }
        Condition::NotIn { field, values } => {
            let v = as_value(resolve_field(field, ctx));
            let m = !values.iter().any(|x| values_eq(&v, x));
            RuleEvaluation { matched: m, reason: if m { String::new() } else { format!("not_in: {:?} in list", v) } }
        }
        Condition::Contains { field, value } => str_cond(field, value, ctx, |a, b| a.contains(b), "contains"),
        Condition::StartsWith { field, value } => str_cond(field, value, ctx, |a, b| a.starts_with(b), "starts_with"),
        Condition::EndsWith { field, value } => str_cond(field, value, ctx, |a, b| a.ends_with(b), "ends_with"),
        Condition::Between { field, min, max } => {
            let v = num(as_value(resolve_field(field, ctx)));
            let lo = num(min.clone());
            let hi = num(max.clone());
            let m = v >= lo && v <= hi;
            RuleEvaluation { matched: m, reason: if m { String::new() } else { format!("between: {} not in [{}, {}]", v, lo, hi) } }
        }
        Condition::Exists { field } => {
            let m = !resolve_field(field, ctx).is_null_or_missing();
            RuleEvaluation { matched: m, reason: if m { String::new() } else { "exists: value is null/missing".into() } }
        }
        Condition::NotExists { field } => {
            let m = resolve_field(field, ctx).is_null_or_missing();
            RuleEvaluation { matched: m, reason: if m { String::new() } else { "not_exists: value present".into() } }
        }
    }
}

/// Rule-level eligibility helper combining condition + status + event type.
pub fn rule_eligible(cond: &Condition, ctx: &Context) -> RuleEvaluation {
    evaluate_condition(cond, ctx)
}

// ── operand resolution ────────────────────────────────────────────────────────

enum Resolved {
    Value(Value),
    /// Unresolvable path.
    Missing(String),
}

impl std::fmt::Debug for Resolved {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Resolved::Value(v) => write!(f, "{:?}", v),
            Resolved::Missing(p) => write!(f, "missing({})", p),
        }
    }
}

impl Resolved {
    fn is_null_or_missing(&self) -> bool {
        match self {
            Resolved::Value(Value::Null) => true,
            Resolved::Missing(_) => true,
            _ => false,
        }
    }
}

/// FIELD operands are always context paths; an unresolvable path is Missing
/// (null semantics per §13).
fn resolve_field(op: &Operand, ctx: &Context) -> Resolved {
    match op {
        Operand::Value(v) => Resolved::Value(v.clone()),
        Operand::Path(p) => match resolve_path(&ctx.values, p) {
            Some(v) => Resolved::Value(v),
            None => Resolved::Missing(p.clone()),
        },
    }
}

/// VALUE operands are literals by default. A string that happens to contain
/// dots ("lesson.completed") is a LITERAL unless it resolves as a context
/// path (dynamic comparison). Found in E2E: symmetric path resolution made
/// every dotted string value a missing path -> all eq conditions failed.
fn resolve_value(op: &Operand, ctx: &Context) -> Resolved {
    match op {
        Operand::Value(v) => Resolved::Value(v.clone()),
        Operand::Path(p) => match resolve_path(&ctx.values, p) {
            Some(v) => Resolved::Value(v),
            None => Resolved::Value(Value::String(p.clone())),
        },
    }
}

fn as_value(r: Resolved) -> Value {
    match r {
        Resolved::Value(v) => v,
        Resolved::Missing(p) => {
            let _ = p;
            Value::Null
        }
    }
}

// ── comparison machinery ──────────────────────────────────────────────────────

fn cmp_cond(
    field: &Operand,
    value: &Operand,
    ctx: &Context,
    ok: impl Fn(i32) -> bool,
    label: &str,
) -> RuleEvaluation {
    let l = resolve_field(field, ctx);
    let r = resolve_value(value, ctx);

    // Null/missing semantics: comparisons with null are false except neq
    // (null != concrete is true when the field exists-but-null vs literal?).
    // Canonical choice: null compares false for everything except neq (true)
    // when one side is null and the other is not.
    if l.is_null_or_missing() || r.is_null_or_missing() {
        let is_neq = label == "neq";
        let both_null = l.is_null_or_missing() && r.is_null_or_missing();
        let matched = if is_neq { !both_null } else { false };
        return RuleEvaluation {
            matched,
            reason: format!("{}: null operand (l={:?}, r={:?})", label, as_value(l), as_value(r)),
        };
    }

    let lv = as_value(l);
    let rv = as_value(r);
    let ord = compare(&lv, &rv);
    let matched = ok(ord);
    RuleEvaluation {
        matched,
        reason: if matched { String::new() } else { format!("{}: {:?} vs {:?}", label, lv, rv) },
    }
}

fn compare(l: &Value, r: &Value) -> i32 {
    // Numbers compare numerically (5 == 5.0).
    if let (Some(a), Some(b)) = (as_f64(l), as_f64(r)) {
        if l.is_number() && r.is_number() {
            return ord_f64(a, b);
        }
    }
    if let (Value::String(a), Value::String(b)) = (l, r) {
        return match a.cmp(b) {
            std::cmp::Ordering::Less => -1,
            std::cmp::Ordering::Equal => 0,
            std::cmp::Ordering::Greater => 1,
        };
    }
    match (l, r) {
        (Value::Bool(a), Value::Bool(b)) => {
            if a == b {
                0
            } else if *a {
                1
            } else {
                -1
            }
        }
        _ => 0,
    }
}

fn ord_f64(a: f64, b: f64) -> i32 {
    if a < b {
        -1
    } else if a > b {
        1
    } else {
        0
    }
}

fn str_cond(
    field: &Operand,
    value: &Operand,
    ctx: &Context,
    ok: fn(&str, &str) -> bool,
    label: &str,
) -> RuleEvaluation {
    let l = match as_value(resolve_field(field, ctx)) {
        Value::String(s) => s,
        other => {
            return RuleEvaluation { matched: false, reason: format!("{}: field {:?} not a string", label, other) }
        }
    };
    let r = match as_value(resolve_value(value, ctx)) {
        Value::String(s) => s,
        other => {
            return RuleEvaluation { matched: false, reason: format!("{}: value {:?} not a string", label, other) }
        }
    };
    let matched = ok(&l, &r);
    RuleEvaluation { matched, reason: if matched { String::new() } else { format!("{}: `{}` vs `{}`", label, l, r) } }
}

fn values_eq(a: &Value, b: &Value) -> bool {
    if a.is_number() && b.is_number() {
        return as_f64(a) == as_f64(b);
    }
    a == b
}

fn as_f64(v: &Value) -> Option<f64> {
    match v {
        Value::Number(n) => n.as_f64(),
        Value::Bool(b) => Some(if *b { 1.0 } else { 0.0 }),
        _ => None,
    }
}

fn num(v: Value) -> f64 {
    as_f64(&v).unwrap_or(0.0)
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn ctx() -> Context {
        Context::default()
            .set("user.level", json!(5))
            .set("user.tier", json!("pro"))
            .set("event.type", json!("lesson.completed"))
            .set("event.payload.points", json!(25))
            .set("user.name", json!("Alice"))
            .set("user.none", Value::Null)
    }

    fn cond(c: Condition) -> RuleEvaluation {
        evaluate_condition(&c, &ctx())
    }

    #[test]
    fn comparisons() {
        assert!(cond(Condition::Eq { field: Operand::path("event.type"), value: Operand::value(json!("lesson.completed")) }).matched);
        assert!(cond(Condition::Gte { field: Operand::path("user.level"), value: Operand::value(json!(5)) }).matched);
        assert!(!cond(Condition::Gt { field: Operand::path("user.level"), value: Operand::value(json!(5)) }).matched);
        assert!(cond(Condition::Lt { field: Operand::path("user.level"), value: Operand::value(json!(6)) }).matched);
    }

    #[test]
    fn wire_shaped_dotted_value_is_literal() {
        // Regression (E2E): on the wire, "value":"lesson.completed"
        // deserializes (untagged) to Operand::Path — it must still compare as
        // a LITERAL string, not a missing path. Fields stay paths.
        let c = Condition::Eq {
            field: Operand::path("event.type"),
            value: Operand::path("lesson.completed"),
        };
        assert!(cond(c).matched);

        // Dynamic comparison still works when the value path RESOLVES:
        let c2 = Condition::Eq {
            field: Operand::path("event.type"),
            value: Operand::path("event.type"),
        };
        assert!(cond(c2).matched);

        // Unresolvable dotted field stays missing (not a literal):
        let c3 = Condition::Gte { field: Operand::path("user.missing.level"), value: Operand::path("5") };
        let r = cond(c3);
        assert!(!r.matched);
    }

    #[test]
    fn bare_literal_field_is_string() {
        // Lesson from previous session: "important" is a literal, never a path.
        let c = Condition::Eq {
            field: Operand::path("event.category"),
            value: Operand::path("important"),
        };
        // event.category missing -> null; "important" literal string -> not equal.
        let r = cond(c);
        assert!(!r.matched);
        let mut ctx = ctx();
        ctx.values.insert("event.category".into(), json!("important"));
        let r = evaluate_condition(
            &Condition::Eq { field: Operand::path("event.category"), value: Operand::path("important") },
            &ctx,
        );
        assert!(r.matched);
    }

    #[test]
    fn null_semantics() {
        // user.none is explicit null.
        let r = cond(Condition::Gt { field: Operand::path("user.none"), value: Operand::value(json!(1)) });
        assert!(!r.matched);
        let r = cond(Condition::Neq { field: Operand::path("user.none"), value: Operand::value(json!(1)) });
        assert!(r.matched);
        let r = cond(Condition::Exists { field: Operand::path("user.none") });
        assert!(!r.matched);
        let r = cond(Condition::NotExists { field: Operand::path("user.none") });
        assert!(r.matched);
    }

    #[test]
    fn membership_and_strings() {
        assert!(cond(Condition::In { field: Operand::path("user.tier"), values: vec![json!("pro"), json!("elite")] }).matched);
        assert!(cond(Condition::NotIn { field: Operand::path("user.tier"), values: vec![json!("free")] }).matched);
        assert!(cond(Condition::Contains { field: Operand::path("user.name"), value: Operand::value(json!("lic")) }).matched);
        assert!(cond(Condition::StartsWith { field: Operand::path("user.name"), value: Operand::value(json!("Al")) }).matched);
        assert!(cond(Condition::EndsWith { field: Operand::path("user.name"), value: Operand::value(json!("ce")) }).matched);
    }

    #[test]
    fn between_and_nesting() {
        assert!(cond(Condition::Between {
            field: Operand::path("user.level"),
            min: json!(1),
            max: json!(10),
        })
        .matched);

        let tree = Condition::And {
            all: vec![
                Condition::Or {
                    any: vec![
                        Condition::Eq { field: Operand::path("user.tier"), value: Operand::value(json!("pro")) },
                        Condition::Gte { field: Operand::path("user.level"), value: Operand::value(json!(100)) },
                    ],
                },
                Condition::Not { condition: Box::new(Condition::Lt { field: Operand::path("user.level"), value: Operand::value(json!(5)) }) },
            ],
        };
        assert!(cond(tree).matched);
    }

    #[test]
    fn failed_evaluation_has_reason() {
        let r = cond(Condition::Gt { field: Operand::path("user.level"), value: Operand::value(json!(99)) });
        assert!(!r.matched);
        assert!(!r.reason.is_empty());
    }
}
