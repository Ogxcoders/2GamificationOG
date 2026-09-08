//! Canonical condition tree (§13 Rule Engine operators).
//!
//! Type-tagged JSON so the Go compiler, Rust evaluator, and SDKs all share one
//! unambiguous wire format (§81 canonical schema pipeline).

use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum Condition {
    // ── composition ──
    And {
        #[serde(default, deserialize_with = "crate::null_vec::deserialize")]
        all: Vec<Condition>,
    },
    Or {
        #[serde(default, deserialize_with = "crate::null_vec::deserialize")]
        any: Vec<Condition>,
    },
    Not {
        condition: Box<Condition>,
    },

    // ── comparisons ──
    Eq {
        field: Operand,
        value: Operand,
    },
    Neq {
        field: Operand,
        value: Operand,
    },
    Gt {
        field: Operand,
        value: Operand,
    },
    Lt {
        field: Operand,
        value: Operand,
    },
    Gte {
        field: Operand,
        value: Operand,
    },
    Lte {
        field: Operand,
        value: Operand,
    },

    // ── membership ──
    In {
        field: Operand,
        values: Vec<serde_json::Value>,
    },
    NotIn {
        field: Operand,
        values: Vec<serde_json::Value>,
    },

    // ── string ──
    Contains {
        field: Operand,
        value: Operand,
    },
    StartsWith {
        field: Operand,
        value: Operand,
    },
    EndsWith {
        field: Operand,
        value: Operand,
    },

    // ── range / existence ──
    Between {
        field: Operand,
        min: serde_json::Value,
        max: serde_json::Value,
    },
    Exists {
        field: Operand,
    },
    NotExists {
        field: Operand,
    },
}

/// Operand: either a literal value or a dotted context-path reference.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(untagged)]
pub enum Operand {
    Path(String),
    Value(serde_json::Value),
}

impl Operand {
    pub fn path(s: impl Into<String>) -> Self {
        Operand::Path(s.into())
    }
    pub fn value(v: serde_json::Value) -> Self {
        Operand::Value(v)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn roundtrips_type_tagged_tree() {
        let raw = r#"{
            "type":"and",
            "all":[
                {"type":"eq","field":"event.type","value":"lesson.completed"},
                {"type":"gte","field":"user.level","value":5},
                {"type":"not","condition":{"type":"in","field":"user.tier","values":["banned","suspended"]}}
            ]
        }"#;
        let c: Condition = serde_json::from_str(raw).unwrap();
        match &c {
            Condition::And { all } => assert_eq!(all.len(), 3),
            _ => panic!("expected and"),
        }
        let s = serde_json::to_string(&c).unwrap();
        assert!(s.contains(r#""type":"and""#));
    }

    #[test]
    fn operand_untagged_forms() {
        let o: Operand = serde_json::from_str(r#""user.level""#).unwrap();
        assert_eq!(o, Operand::Path("user.level".into()));
        let o: Operand = serde_json::from_str("5").unwrap();
        assert_eq!(o, Operand::Value(serde_json::json!(5)));
    }
}
