//! # platform-formulas
//!
//! Deterministic, sandboxed expression engine (§14 + §381 safety boundaries).
//!
//! - Grammar: `expr := term (("+"|"-") term)*`, `term := unary (("*"|"/"|"%") unary)*`,
//!   `unary := ("-"|"!")? primary`, `primary := number | string | ident | "(" expr ")"
//!   | func "(" args ")" | ident (":" | comparisons for booleans)`
//! - Boolean operators: `and`, `or`, `not` keywords plus `&&`, `||`, `!`.
//! - Comparisons: `==`, `!=`, `<`, `<=`, `>`, `>=` produce booleans.
//! - Functions: whitelist only — `min, max, abs, floor, ceil, round, clamp, pow, sqrt`.
//! - Variables resolve against the flat context scope with nested fallback
//!   (`event.payload.points`) — longest-prefix semantics via `resolve_path`.
//! - **Determinism**: no wall-clock, no RNG, no I/O; numeric output is normalized
//!   so whole floats serialize as JSON integers (i64-safe — the previous
//!   implementation famously failed `as_i64()` on `250.0`).
//! - Execution limits: token count, node count, and recursion depth caps.

pub mod lexer;
pub mod parser;
pub mod eval;

pub use eval::{evaluate, evaluate_f64, with_limits, EvalLimits};
pub use parser::{parse, Expr};

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    use std::collections::HashMap;

    fn scope() -> HashMap<String, serde_json::Value> {
        let mut m = HashMap::new();
        m.insert("base_xp".into(), json!(100));
        m.insert("multiplier".into(), json!(1.5));
        m.insert("level".into(), json!(5));
        m.insert("daily_cap".into(), json!(1000));
        m.insert("event".into(), json!({"payload": {"points": 250}}));
        m
    }

    #[test]
    fn arithmetic_and_precedence() {
        let v = evaluate("2 + 3 * 4", &scope()).unwrap();
        assert_eq!(v, json!(14));
        let v = evaluate("(2 + 3) * 4", &scope()).unwrap();
        assert_eq!(v, json!(20));
    }

    #[test]
    fn variables_and_nested_paths() {
        let v = evaluate("base_xp * multiplier", &scope()).unwrap();
        assert_eq!(v, json!(150)); // whole float -> integer JSON number
        let v = evaluate("event.payload.points", &scope()).unwrap();
        assert_eq!(v, json!(250));
    }

    #[test]
    fn functions() {
        assert_eq!(evaluate("min(level * 100, daily_cap)", &scope()).unwrap(), json!(500));
        assert_eq!(evaluate("max(1, 2, 3)", &scope()).unwrap(), json!(3));
        assert_eq!(evaluate("abs(0 - 7)", &scope()).unwrap(), json!(7));
        assert_eq!(evaluate("floor(2.7)", &scope()).unwrap(), json!(2));
        assert_eq!(evaluate("ceil(2.1)", &scope()).unwrap(), json!(3));
        assert_eq!(evaluate("round(2.5)", &scope()).unwrap(), json!(3));
        assert_eq!(evaluate("clamp(15, 0, 10)", &scope()).unwrap(), json!(10));
        assert_eq!(evaluate("pow(2, 10)", &scope()).unwrap(), json!(1024));
        assert_eq!(evaluate("sqrt(144)", &scope()).unwrap(), json!(12));
    }

    #[test]
    fn booleans_and_comparisons() {
        assert_eq!(evaluate("level >= 5 and base_xp > 50", &scope()).unwrap(), json!(true));
        assert_eq!(evaluate("level < 5 or daily_cap == 1000", &scope()).unwrap(), json!(true));
        assert_eq!(evaluate("not (level < 5)", &scope()).unwrap(), json!(true));
        assert_eq!(evaluate("!(level < 5)", &scope()).unwrap(), json!(true));
        assert_eq!(evaluate("level != 4", &scope()).unwrap(), json!(true));
    }

    #[test]
    fn division_and_modulo() {
        assert_eq!(evaluate("7 / 2", &scope()).unwrap(), json!(3.5));
        assert_eq!(evaluate("7 % 3", &scope()).unwrap(), json!(1));
    }

    #[test]
    fn division_by_zero_is_a_formula_error() {
        let e = evaluate("1 / 0", &scope()).unwrap_err();
        assert!(matches!(e, platform_common::EngineError::Formula { .. }));
    }

    #[test]
    fn unknown_variable_is_null_not_error() {
        assert_eq!(evaluate("missing_var", &scope()).unwrap(), json!(null));
        assert_eq!(evaluate("missing_var + 1", &scope()).unwrap(), json!(1)); // null -> 0
    }

    #[test]
    fn deep_nesting_hits_limit() {
        let expr = "(".repeat(200).to_string() + "1" + &")".repeat(200);
        assert!(evaluate(&expr, &scope()).is_err());
    }

    #[test]
    fn oversized_expression_rejected() {
        let expr: String = std::iter::repeat("1+").take(20_000).collect::<String>() + "1";
        assert!(evaluate(&expr, &scope()).is_err());
    }

    #[test]
    fn whole_floats_serialize_as_integers() {
        // The classic bug: Number(250.0) must not lose i64-ness.
        let v = evaluate("250.0", &scope()).unwrap();
        assert!(v.is_i64());
        assert_eq!(v.as_i64(), Some(250));
        let v = evaluate("250.5", &scope()).unwrap();
        assert!(v.is_f64());
    }

    #[test]
    fn evaluate_f64_helper() {
        assert_eq!(evaluate_f64("base_xp * multiplier", &scope()).unwrap(), 150.0);
        // null operand -> 0
        assert_eq!(evaluate_f64("missing + 2", &scope()).unwrap(), 2.0);
    }

    #[test]
    fn unary_minus_and_not() {
        assert_eq!(evaluate("-level", &scope()).unwrap(), json!(-5));
        assert_eq!(evaluate("- 2 + 10", &scope()).unwrap(), json!(8));
        assert_eq!(evaluate("not true", &scope()).unwrap(), json!(false));
    }
}
