//! Operand path resolution (shared by rules + formulas).
//!
//! Semantics (lesson from the previous session's silent-rule-bug):
//! a string operand is a **path reference** only when it contains a dot AND
//! resolves to a value in the scope. Bare literals like `"important"` are
//! literal strings. Nested JSON objects are traversed when the scope value is
//! an object (longest-prefix match for flat dotted keys).

use serde_json::Value;

/// Resolve `a.b.c` against a scope that stores flat dotted keys and/or
/// nested objects.
///
/// Resolution order:
/// 1. Exact flat-key match (`event.payload.points` stored as one key).
/// 2. Longest-prefix match: find the longest stored key that prefixes the path,
///    then walk the remaining segments through the nested value.
/// 3. Walk from the first segment (scope holding full nested objects).
pub fn resolve_path(scope: &std::collections::HashMap<String, Value>, path: &str) -> Option<Value> {
    if path.is_empty() {
        return None;
    }
    // 1. Exact.
    if let Some(v) = scope.get(path) {
        return Some(v.clone());
    }
    let segments: Vec<&str> = path.split('.').collect();
    if segments.len() < 2 {
        return None;
    }
    // 2. Longest stored prefix + nested walk.
    for take in (1..segments.len()).rev() {
        let prefix = segments[..take].join(".");
        if let Some(Value::Object(map)) = scope.get(&prefix) {
            if let Some(v) = walk(map, &segments[take..]) {
                return Some(v);
            }
        }
    }
    // 3. Fully nested root.
    if let Some(Value::Object(map)) = scope.get(segments[0]) {
        if let Some(v) = walk(map, &segments[1..]) {
            return Some(v);
        }
    }
    None
}

fn walk(map: &serde_json::Map<String, Value>, segments: &[&str]) -> Option<Value> {
    let mut cur = map.get(segments[0])?.clone();
    for seg in &segments[1..] {
        match cur {
            Value::Object(ref m) => cur = m.get(*seg)?.clone(),
            _ => return None,
        }
    }
    Some(cur)
}

/// True when the string should be treated as a *candidate* path reference
/// (contains a dot). The caller still requires successful resolution before
/// preferring the path interpretation over a literal.
pub fn looks_like_path(s: &str) -> bool {
    s.contains('.')
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    use std::collections::HashMap;

    fn scope() -> HashMap<String, Value> {
        let mut m = HashMap::new();
        m.insert("user.level".into(), json!(7));
        m.insert("event".into(), json!({"type": "lesson.completed", "payload": {"points": 25}}));
        m.insert("event.type".into(), json!("lesson.completed"));
        m
    }

    #[test]
    fn exact_flat_key() {
        assert_eq!(resolve_path(&scope(), "user.level"), Some(json!(7)));
    }

    #[test]
    fn longest_prefix_beats_shorter() {
        // `event.type` exists flat AND nested; flat exact wins.
        assert_eq!(
            resolve_path(&scope(), "event.type"),
            Some(json!("lesson.completed"))
        );
        // Nested walk through the object root.
        assert_eq!(resolve_path(&scope(), "event.payload.points"), Some(json!(25)));
    }

    #[test]
    fn bare_literal_is_not_a_path() {
        assert!(!looks_like_path("important"));
        assert!(looks_like_path("user.level"));
        // "important" has no dot and no scope entry — resolution fails -> literal.
        assert_eq!(resolve_path(&scope(), "important"), None);
    }

    #[test]
    fn dotted_unresolvable_returns_none() {
        assert_eq!(resolve_path(&scope(), "user.level.nope"), None);
        assert_eq!(resolve_path(&scope(), "event"), Some(json!({"type": "lesson.completed", "payload": {"points": 25}})));
    }
}
