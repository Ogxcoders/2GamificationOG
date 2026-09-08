//! # platform-common
//!
//! Canonical domain primitives shared by every engine crate:
//! typed IDs, the Time Engine (§18, §207), the error taxonomy (§106),
//! the canonical event contract (§10), processing context (§12),
//! engine-facing configuration types (§6/§7) and decision-trace primitives (§229/§230).
//!
//! Deserialization is null-tolerant everywhere: the Go control plane serializes
//! nil slices/maps as JSON `null`, and `#[serde(default)]` alone does not accept
//! an explicit `null`, so every collection field goes through `null_or_default`.

pub mod ids;
pub mod time;
pub mod errors;
pub mod event;
pub mod context;
pub mod condition;
pub mod config;
pub mod trace;
pub mod value;

pub use errors::{EngineError, EngineResult};
pub use event::CanonicalEvent;
pub use ids::*;
pub use time::{resolve_window, TimeWindow, CalendarUnit, ResolvedWindow};
pub use value::resolve_path;
pub use condition::{Condition, Operand};
pub use context::Context;
pub use config::*;
pub use trace::{DecisionTrace, TraceNode, TraceNodeKind};

use serde::{Deserialize, Deserializer, Serialize};

/// Lifecycle status shared by all configurable objects (§7).
/// Authored lifecycle vs runtime status are distinct: `published` is the
/// authored terminal state; the control plane materializes published objects
/// into runtime tables with status `active`.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum ObjectStatus {
    Draft,
    Validated,
    Preview,
    Approved,
    Published,
    Scheduled,
    Active,
    Paused,
    Archived,
}

impl Default for ObjectStatus {
    fn default() -> Self {
        ObjectStatus::Draft
    }
}

/// Map an authored lifecycle status onto the runtime status the pipeline
/// should use when materializing objects into runtime tables.
pub fn runtime_status(authored: ObjectStatus) -> ObjectStatus {
    match authored {
        ObjectStatus::Published | ObjectStatus::Scheduled => ObjectStatus::Active,
        ObjectStatus::Paused => ObjectStatus::Paused,
        other => other,
    }
}

/// Null-tolerant Vec deserializer (Go encodes nil slices as null).
pub mod null_vec {
    use serde::{Deserialize, Deserializer, Serialize, Serializer};

    pub fn deserialize<'de, D, T>(d: D) -> Result<Vec<T>, D::Error>
    where
        D: Deserializer<'de>,
        T: Deserialize<'de>,
    {
        let opt: Option<Vec<T>> = Option::deserialize(d)?;
        Ok(opt.unwrap_or_default())
    }

    pub fn serialize<S: Serializer, T: Serialize>(v: &Vec<T>, s: S) -> Result<S::Ok, S::Error> {
        if v.is_empty() {
            s.serialize_none()
        } else {
            v.serialize(s)
        }
    }
}

/// Null-tolerant Option already handles null; helper for maps (Go nil maps -> null).
pub mod null_map {
    use serde::{Deserialize, Deserializer};
    use std::collections::HashMap;

    pub fn deserialize<'de, D, T>(d: D) -> Result<HashMap<String, T>, D::Error>
    where
        D: Deserializer<'de>,
        T: Deserialize<'de>,
    {
        let opt: Option<HashMap<String, T>> = Option::deserialize(d)?;
        Ok(opt.unwrap_or_default())
    }
}

/// A JSON value that also accepts null and normalizes it to a default.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize, Default)]
pub struct NullTolerantValue(#[serde(default, deserialize_with = "opt_value")] pub serde_json::Value);

fn opt_value<'de, D>(d: D) -> Result<serde_json::Value, D::Error>
where
    D: Deserializer<'de>,
{
    let v: Option<serde_json::Value> = Option::deserialize(d)?;
    Ok(v.unwrap_or(serde_json::Value::Null))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn null_vec_accepts_null() {
        #[derive(Deserialize)]
        struct S {
            #[serde(default, deserialize_with = "null_vec::deserialize")]
            items: Vec<i64>,
        }
        let s: S = serde_json::from_str(r#"{"items":null}"#).unwrap();
        assert!(s.items.is_empty());
        let s: S = serde_json::from_str("{}").unwrap();
        assert!(s.items.is_empty());
        let s: S = serde_json::from_str(r#"{"items":[1,2]}"#).unwrap();
        assert_eq!(s.items, vec![1, 2]);
    }

    #[test]
    fn runtime_status_mapping() {
        assert_eq!(runtime_status(ObjectStatus::Published), ObjectStatus::Active);
        assert_eq!(runtime_status(ObjectStatus::Draft), ObjectStatus::Draft);
    }
}
