//! Error taxonomy (§106 Error Experience + §179 retry classification).
//!
//! Every error is human-readable, actionable, structured, traceable.
//! Retry classification drives bus/worker behavior in the control plane.

use thiserror::Error;

#[derive(Debug, Error)]
pub enum EngineError {
    /// Deterministic validation failure — never retried.
    #[error("validation failed: {field}: {message}")]
    Validation { field: String, message: String },

    /// Unknown or malformed reference (e.g. currency does not exist).
    #[error("reference not found: {reference} — {hint}")]
    ReferenceNotFound { reference: String, hint: String },

    /// Expression/formula errors — deterministic.
    #[error("formula error in `{expression}`: {message}")]
    Formula { expression: String, message: String },

    /// Economy invariant violation (negative balance, cap, double spend).
    #[error("economy violation: {message} (balance_after={balance_after:?})")]
    Economy { message: String, balance_after: Option<i64> },

    /// Non-deterministic / infrastructure failure — retried with backoff.
    #[error("transient failure: {message}")]
    Transient { message: String },

    /// Unsupported capability — configuration references a missing capability.
    #[error("unsupported: {message}")]
    Unsupported { message: String },

    #[error("serde error: {0}")]
    Serde(#[from] serde_json::Error),
}

pub type EngineResult<T> = Result<T, EngineError>;

impl EngineError {
    /// Retry classification per §179: validation/reference/formula/economy are
    /// deterministic failures; only transient errors are retryable.
    pub fn retryable(&self) -> bool {
        matches!(self, EngineError::Transient { .. })
    }

    /// Stable machine code for logs/audit.
    pub fn code(&self) -> &'static str {
        match self {
            EngineError::Validation { .. } => "validation_failed",
            EngineError::ReferenceNotFound { .. } => "reference_not_found",
            EngineError::Formula { .. } => "formula_error",
            EngineError::Economy { .. } => "economy_violation",
            EngineError::Transient { .. } => "transient_failure",
            EngineError::Unsupported { .. } => "unsupported",
            EngineError::Serde(_) => "serialization_error",
        }
    }

    pub fn validation(field: impl Into<String>, message: impl Into<String>) -> Self {
        EngineError::Validation {
            field: field.into(),
            message: message.into(),
        }
    }

    pub fn formula(expression: impl Into<String>, message: impl Into<String>) -> Self {
        EngineError::Formula {
            expression: expression.into(),
            message: message.into(),
        }
    }

    pub fn transient(message: impl Into<String>) -> Self {
        EngineError::Transient {
            message: message.into(),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn retry_classification() {
        assert!(!EngineError::validation("x", "bad").retryable());
        assert!(EngineError::transient("db down").retryable());
    }

    #[test]
    fn codes_are_stable() {
        assert_eq!(EngineError::validation("x", "m").code(), "validation_failed");
    }
}
