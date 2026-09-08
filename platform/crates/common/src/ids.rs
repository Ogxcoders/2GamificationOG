//! Typed identifier helpers. IDs are opaque strings; the macro generates a
//! newtype so cross-usage is a compile error, plus `Default` support so
//! serde `#[serde(default)]` works on response structs.

use serde::{Deserialize, Serialize};
use std::fmt;

/// Generate a typed ID newtype.
macro_rules! typed_id {
    ($name:ident) => {
        #[derive(
            Debug, Clone, PartialEq, Eq, Hash, Serialize, Deserialize, Default, PartialOrd, Ord,
        )]
        #[serde(transparent)]
        pub struct $name(pub String);

        impl $name {
            pub fn new(s: impl Into<String>) -> Self {
                $name(s.into())
            }
            pub fn as_str(&self) -> &str {
                &self.0
            }
            pub fn is_empty(&self) -> bool {
                self.0.is_empty()
            }
        }

        impl fmt::Display for $name {
            fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
                f.write_str(&self.0)
            }
        }

        impl From<&str> for $name {
            fn from(s: &str) -> Self {
                $name(s.to_string())
            }
        }
    };
}

typed_id!(UserId);
typed_id!(ProjectId);
typed_id!(EnvironmentId);
typed_id!(OrgId);
typed_id!(EventId);
typed_id!(RuleId);
typed_id!(ChallengeId);
typed_id!(AchievementId);
typed_id!(StreakId);
typed_id!(CurrencyId);
typed_id!(LeaderboardId);
typed_id!(RewardId);
typed_id!(LevelTrackId);
typed_id!(WorkflowId);
typed_id!(SegmentId);
typed_id!(SeasonId);
typed_id!(ItemId);
typed_id!(CorrelationId);
typed_id!(TraceId);

/// Generate a fresh event/trace UUID.
pub fn new_uuid() -> String {
    uuid::Uuid::new_v4().to_string()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ids_are_typed_and_defaultable() {
        #[derive(Deserialize, Default)]
        struct S {
            #[serde(default)]
            user: UserId,
        }
        let s: S = serde_json::from_str("{}").unwrap();
        assert!(s.user.is_empty());
        let s: S = serde_json::from_str(r#"{"user":"u1"}"#).unwrap();
        assert_eq!(s.user, UserId::new("u1"));
    }
}
