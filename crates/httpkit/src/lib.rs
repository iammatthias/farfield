//! `farfield-httpkit` — HTTP scaffolding shared by every app service.
//!
//! Two things, deliberately small: a uniform [`ApiError`] type that renders
//! as `{ "error", "message" }` JSON, and constant-time bearer-token
//! verification. The records service and the blob service both build on it.

use axum::Json;
use axum::http::{HeaderMap, StatusCode, header};
use axum::response::{IntoResponse, Response};
use serde_json::json;

/// A uniform API error: an HTTP status, a stable machine code, and a human
/// message. Renders as `{ "error": <code>, "message": <message> }`.
#[derive(Debug, Clone)]
pub struct ApiError {
    pub status: StatusCode,
    pub code: &'static str,
    pub message: String,
}

impl ApiError {
    pub fn new(status: StatusCode, code: &'static str, message: impl Into<String>) -> Self {
        Self {
            status,
            code,
            message: message.into(),
        }
    }

    /// `404 not_found`.
    pub fn not_found(message: impl Into<String>) -> Self {
        Self::new(StatusCode::NOT_FOUND, "not_found", message)
    }

    /// `400` with a caller-chosen code (e.g. `invalid_record`, `invalid_rkey`).
    pub fn bad_request(code: &'static str, message: impl Into<String>) -> Self {
        Self::new(StatusCode::BAD_REQUEST, code, message)
    }

    /// `401 unauthorized`.
    pub fn unauthorized() -> Self {
        Self::new(
            StatusCode::UNAUTHORIZED,
            "unauthorized",
            "missing or invalid bearer token",
        )
    }

    /// `500 internal`.
    pub fn internal(message: impl Into<String>) -> Self {
        Self::new(StatusCode::INTERNAL_SERVER_ERROR, "internal", message)
    }
}

impl IntoResponse for ApiError {
    fn into_response(self) -> Response {
        (
            self.status,
            Json(json!({ "error": self.code, "message": self.message })),
        )
            .into_response()
    }
}

impl std::fmt::Display for ApiError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{} ({})", self.message, self.code)
    }
}

impl std::error::Error for ApiError {}

/// Verify the request's `Authorization: Bearer <token>` against the accepted
/// set. Comparison is constant-time within a length class. An empty accepted
/// set rejects everything.
pub fn verify_bearer(headers: &HeaderMap, accepted: &[String]) -> Result<(), ApiError> {
    let presented = headers
        .get(header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.strip_prefix("Bearer "))
        .unwrap_or("");
    let mut matched = false;
    for token in accepted {
        if !token.is_empty() && constant_time_eq(presented.as_bytes(), token.as_bytes()) {
            matched = true;
        }
    }
    if matched {
        Ok(())
    } else {
        Err(ApiError::unauthorized())
    }
}

fn constant_time_eq(a: &[u8], b: &[u8]) -> bool {
    if a.len() != b.len() {
        return false;
    }
    let mut diff = 0u8;
    for (x, y) in a.iter().zip(b) {
        diff |= x ^ y;
    }
    diff == 0
}

#[cfg(test)]
mod tests {
    use super::*;

    fn auth(value: &str) -> HeaderMap {
        let mut h = HeaderMap::new();
        h.insert(header::AUTHORIZATION, value.parse().unwrap());
        h
    }

    #[test]
    fn a_correct_token_passes() {
        let accepted = vec!["secret".to_string()];
        assert!(verify_bearer(&auth("Bearer secret"), &accepted).is_ok());
    }

    #[test]
    fn a_wrong_token_is_rejected() {
        let accepted = vec!["secret".to_string()];
        assert!(verify_bearer(&auth("Bearer nope"), &accepted).is_err());
    }

    #[test]
    fn a_previous_token_still_passes_during_rotation() {
        let accepted = vec!["new".to_string(), "old".to_string()];
        assert!(verify_bearer(&auth("Bearer old"), &accepted).is_ok());
    }

    #[test]
    fn a_missing_header_is_rejected() {
        assert!(verify_bearer(&HeaderMap::new(), &["secret".to_string()]).is_err());
    }

    #[test]
    fn an_empty_accepted_set_rejects_everything() {
        assert!(verify_bearer(&auth("Bearer anything"), &[]).is_err());
    }
}
