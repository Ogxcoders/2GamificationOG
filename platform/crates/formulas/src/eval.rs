//! Evaluator: Expr + scope -> serde_json::Value, with determinism guarantees.

use crate::parser::{BinOp, Expr, UnOp};
use platform_common::{EngineError, EngineResult};
use serde_json::Value;
use std::collections::HashMap;

/// Execution limits applied on top of the parser's structural caps.
#[derive(Debug, Clone, Copy)]
pub struct EvalLimits {
    /// Total function-call budget (prevents Billion-laughs-style amplification).
    pub max_calls: usize,
}

impl Default for EvalLimits {
    fn default() -> Self {
        EvalLimits { max_calls: 1_000 }
    }
}

thread_local! {
    static CALL_BUDGET: std::cell::Cell<usize> = const { std::cell::Cell::new(1_000) };
}

/// Evaluate with custom limits.
pub fn with_limits(expr: &str, scope: &HashMap<String, Value>, limits: EvalLimits) -> EngineResult<Value> {
    CALL_BUDGET.with(|c| c.set(limits.max_calls));
    let ast = crate::parser::parse(expr)?;
    eval(&ast, scope)
}

/// Evaluate an expression against a flat dotted-key scope.
pub fn evaluate(expr: &str, scope: &HashMap<String, Value>) -> EngineResult<Value> {
    with_limits(expr, scope, EvalLimits::default())
}

/// Evaluate and coerce to f64 (null -> 0.0, booleans -> 0/1).
pub fn evaluate_f64(expr: &str, scope: &HashMap<String, Value>) -> EngineResult<f64> {
    let v = evaluate(expr, scope)?;
    Ok(to_f64(&v).unwrap_or(0.0))
}

fn eval(e: &Expr, scope: &HashMap<String, Value>) -> EngineResult<Value> {
    match e {
        Expr::Number(v) => Ok(number(*v)),
        Expr::Str(s) => Ok(Value::String(s.clone())),
        Expr::Bool(b) => Ok(Value::Bool(*b)),
        Expr::Variable(name) => Ok(platform_common::resolve_path(scope, name).unwrap_or(Value::Null)),
        Expr::Unary { op, operand } => {
            let v = eval(operand, scope)?;
            match op {
                UnOp::Neg => {
                    let f = to_f64(&v).unwrap_or(0.0);
                    Ok(number(-f))
                }
                UnOp::Not => Ok(Value::Bool(!truthy(&v))),
            }
        }
        Expr::Binary { op, left, right } => {
            let l = eval(left, scope)?;
            let r = eval(right, scope)?;
            let out = match op {
                BinOp::And => Value::Bool(truthy(&l) && truthy(&r)),
                BinOp::Or => Value::Bool(truthy(&l) || truthy(&r)),
                BinOp::Eq => Value::Bool(json_eq(&l, &r)),
                BinOp::Neq => Value::Bool(!json_eq(&l, &r)),
                BinOp::Lt | BinOp::Lte | BinOp::Gt | BinOp::Gte => {
                    let (a, b) = comparable(&l, &r);
                    let res = match op {
                        BinOp::Lt => a < b,
                        BinOp::Lte => a <= b,
                        BinOp::Gt => a > b,
                        BinOp::Gte => a >= b,
                        _ => unreachable!(),
                    };
                    Value::Bool(res)
                }
                BinOp::Add => {
                    // String + string concatenation; otherwise numeric.
                    if let (Value::String(a), Value::String(b)) = (&l, &r) {
                        Value::String(format!("{}{}", a, b))
                    } else {
                        number(to_f64(&l).unwrap_or(0.0) + to_f64(&r).unwrap_or(0.0))
                    }
                }
                BinOp::Sub => number(to_f64(&l).unwrap_or(0.0) - to_f64(&r).unwrap_or(0.0)),
                BinOp::Mul => number(to_f64(&l).unwrap_or(0.0) * to_f64(&r).unwrap_or(0.0)),
                BinOp::Div => {
                    let d = to_f64(&r).unwrap_or(0.0);
                    if d == 0.0 {
                        return Err(EngineError::formula(format!("{:?}", op), "division by zero"));
                    }
                    number(to_f64(&l).unwrap_or(0.0) / d)
                }
                BinOp::Mod => {
                    let d = to_f64(&r).unwrap_or(0.0);
                    if d == 0.0 {
                        return Err(EngineError::formula("%", "modulo by zero"));
                    }
                    let a = to_f64(&l).unwrap_or(0.0);
                    number(a - (a / d).floor() * d)
                }
            };
            Ok(out)
        }
        Expr::Call { func, args } => {
            let allowed = CALL_BUDGET.with(|c| {
                let left = c.get();
                if left > 0 {
                    c.set(left - 1);
                    true
                } else {
                    false
                }
            });
            if !allowed {
                return Err(EngineError::formula(func, "evaluation budget exceeded"));
            }
            let vals: Result<Vec<f64>, EngineError> = args
                .iter()
                .map(|a| Ok(to_f64(&eval(a, scope)?).unwrap_or(0.0)))
                .collect();
            let vals = vals?;
            if vals.is_empty() && !matches!(func.as_str(), "min" | "max") {
                return Err(EngineError::formula(func, "function requires arguments"));
            }
            let out = match func.as_str() {
                "min" => vals.iter().cloned().fold(f64::INFINITY, f64::min),
                "max" => vals.iter().cloned().fold(f64::NEG_INFINITY, f64::max),
                "abs" => vals[0].abs(),
                "floor" => vals[0].floor(),
                "ceil" => vals[0].ceil(),
                "round" => vals[0].round(),
                "clamp" => {
                    if vals.len() != 3 {
                        return Err(EngineError::formula(func, "clamp(value, min, max) needs 3 arguments"));
                    }
                    vals[0].min(vals[2]).max(vals[1])
                }
                "pow" => {
                    if vals.len() != 2 {
                        return Err(EngineError::formula(func, "pow(base, exp) needs 2 arguments"));
                    }
                    vals[0].powf(vals[1])
                }
                "sqrt" => {
                    if vals[0] < 0.0 {
                        return Err(EngineError::formula(func, "sqrt of negative number"));
                    }
                    vals[0].sqrt()
                }
                other => {
                    return Err(EngineError::formula(
                        other,
                        format!("unknown function `{}` (whitelist: min max abs floor ceil round clamp pow sqrt)", other),
                    ))
                }
            };
            Ok(number(out))
        }
    }
}

/// Normalize numeric output: whole floats become JSON integers so that
/// consumers can rely on `as_i64()` (determinism + i64-safety).
pub(crate) fn number(v: f64) -> Value {
    if v.is_finite() && v.fract() == 0.0 && v.abs() < 9.007_199_254_740_992e15 {
        Value::from(v as i64)
    } else if v.is_finite() {
        serde_json::Number::from_f64(v)
            .map(Value::Number)
            .unwrap_or(Value::Null)
    } else {
        Value::Null
    }
}

fn to_f64(v: &Value) -> Option<f64> {
    match v {
        Value::Number(n) => n.as_f64(),
        Value::Bool(b) => Some(if *b { 1.0 } else { 0.0 }),
        Value::Null => Some(0.0),
        Value::String(s) => s.parse::<f64>().ok(),
        _ => None,
    }
}

fn truthy(v: &Value) -> bool {
    match v {
        Value::Bool(b) => *b,
        Value::Null => false,
        Value::Number(n) => n.as_f64().map(|f| f != 0.0).unwrap_or(false),
        Value::String(s) => !s.is_empty(),
        Value::Array(a) => !a.is_empty(),
        Value::Object(o) => !o.is_empty(),
    }
}

fn json_eq(l: &Value, r: &Value) -> bool {
    // Numeric cross-type equality (5 == 5.0); strings compare as strings.
    if l.is_number() && r.is_number() {
        return to_f64(l) == to_f64(r);
    }
    l == r
}

/// Comparisons: numbers compare numerically, strings lexicographically.
fn comparable(l: &Value, r: &Value) -> (f64, f64) {
    if let (Some(a), Some(b)) = (to_f64(l), to_f64(r)) {
        if l.is_number() || r.is_number() || l.is_null() || r.is_null() {
            return (a, b);
        }
    }
    if let (Value::String(a), Value::String(b)) = (l, r) {
        // Lexicographic order mapped onto f64 comparator (equal -> 0.0).
        return match a.cmp(b) {
            std::cmp::Ordering::Less => (-1.0, 0.0),
            std::cmp::Ordering::Equal => (0.0, 0.0),
            std::cmp::Ordering::Greater => (1.0, 0.0),
        };
    }
    (0.0, 0.0)
}
