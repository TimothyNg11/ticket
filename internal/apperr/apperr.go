// Package apperr defines errors that know their HTTP status and stable error code.
// Domain packages return these; the HTTP layer renders them. Any other error
// becomes a 500 with no internal details exposed.
package apperr

import "net/http"

// Error is an expected, client-facing failure.
type Error struct {
	Status  int
	Code    string // stable, machine-readable, e.g. "EMAIL_TAKEN"
	Message string // human-readable; safe to show clients
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Validation is a malformed or invalid request (400).
func Validation(msg string) *Error { return &Error{http.StatusBadRequest, "VALIDATION_FAILED", msg} }

// Unauthenticated means credentials are missing or invalid (401).
func Unauthenticated(msg string) *Error {
	return &Error{http.StatusUnauthorized, "UNAUTHENTICATED", msg}
}

// Forbidden means the caller is authenticated but not allowed (403).
func Forbidden() *Error { return &Error{http.StatusForbidden, "FORBIDDEN", "not allowed"} }

// NotFound reports a missing resource (404).
func NotFound(what string) *Error {
	return &Error{http.StatusNotFound, "NOT_FOUND", what + " not found"}
}

// Conflict reports a state conflict such as a duplicate (409).
func Conflict(code, msg string) *Error { return &Error{http.StatusConflict, code, msg} }

// Unprocessable reports a business-rule violation in a well-formed request (422).
func Unprocessable(code, msg string) *Error { return &Error{http.StatusUnprocessableEntity, code, msg} }

// Unavailable reports a dependency outage (503).
func Unavailable(msg string) *Error {
	return &Error{http.StatusServiceUnavailable, "DEPENDENCY_DOWN", msg}
}
