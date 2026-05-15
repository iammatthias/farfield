// Package httpkit is HTTP scaffolding shared by every app service: a uniform
// JSON error type and constant-time bearer-token verification. Built on the
// standard library — no web framework.
package httpkit

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

// APIError is a uniform API error: an HTTP status, a stable machine code, and
// a human message. It renders as { "error": <code>, "message": <message> }.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string { return e.Message + " (" + e.Code + ")" }

// NotFound builds a 404.
func NotFound(message string) *APIError {
	return &APIError{http.StatusNotFound, "not_found", message}
}

// BadRequest builds a 400 with a caller-chosen code.
func BadRequest(code, message string) *APIError {
	return &APIError{http.StatusBadRequest, code, message}
}

// Unauthorized builds a 401.
func Unauthorized() *APIError {
	return &APIError{http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token"}
}

// Internal builds a 500.
func Internal(message string) *APIError {
	return &APIError{http.StatusInternalServerError, "internal", message}
}

// WriteJSON writes v as a JSON response with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes an APIError as a JSON response.
func WriteError(w http.ResponseWriter, e *APIError) {
	WriteJSON(w, e.Status, map[string]any{"error": e.Code, "message": e.Message})
}

// VerifyBearer checks the request's Authorization: Bearer token against the
// accepted set, in constant time. An empty accepted set rejects everything.
func VerifyBearer(r *http.Request, accepted []string) *APIError {
	presented := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	for _, token := range accepted {
		if token != "" && subtle.ConstantTimeCompare([]byte(presented), []byte(token)) == 1 {
			return nil
		}
	}
	return Unauthorized()
}
