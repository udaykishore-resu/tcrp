package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/udaykishore-resu/tcrp/internal/agent"
	"github.com/udaykishore-resu/tcrp/internal/identity"
	"github.com/udaykishore-resu/tcrp/internal/lake"
	"github.com/udaykishore-resu/tcrp/internal/risk"
)

// Error codes. They are a closed set so a client can branch on the code rather
// than on the message, which is written for a human and may change.
const (
	CodeBadRequest      = "bad_request"
	CodeUnauthenticated = "unauthenticated"
	CodeForbidden       = "forbidden"
	CodeNotFound        = "not_found"
	CodeConflict        = "conflict"
	CodeTooLarge        = "payload_too_large"
	CodeRateLimited     = "rate_limited"
	CodeInternal        = "internal"
)

// Error is the body of every failed request. One shape, always, including for a
// panic: a client that has to guess which of three error encodings it got is a
// client that ends up ignoring the body.
type Error struct {
	// Code is the machine-readable class of failure.
	Code string `json:"code"`
	// Message states what went wrong, for a person.
	Message string `json:"message"`
	// RequestID correlates the failure with the access log and the trace.
	RequestID string `json:"request_id,omitempty"`
}

// Error implements the error interface so a handler can return one directly.
func (e Error) Error() string { return e.Code + ": " + e.Message }

// errorEnvelope wraps an Error so the success and failure bodies can never be
// confused: a failure always has exactly one top-level key.
type errorEnvelope struct {
	Error Error `json:"error"`
}

// writeJSON encodes v as the response body.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// writeError renders the one error shape.
func writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	writeJSON(w, status, errorEnvelope{Error{Code: code, Message: msg, RequestID: requestIDOf(r.Context())}})
}

// fail maps a component's sentinel error onto a status and a code.
//
// The mapping lives here rather than in each handler so that "the approver may
// not approve their own action" is a 403 wherever it is raised, and so adding a
// sentinel to a package below forces one edit rather than several.
func fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case err == nil:
		return
	case errors.Is(err, agent.ErrRunNotFound),
		errors.Is(err, agent.ErrApprovalNotFound),
		errors.Is(err, lake.ErrNoSuchBreak),
		errors.Is(err, risk.ErrUnknownCounterparty):
		writeError(w, r, http.StatusNotFound, CodeNotFound, err.Error())
	case errors.Is(err, agent.ErrSelfApproval),
		errors.Is(err, agent.ErrApproverRole),
		errors.Is(err, agent.ErrApproverScope),
		errors.Is(err, identity.ErrNoPrincipal):
		writeError(w, r, http.StatusForbidden, CodeForbidden, err.Error())
	case errors.Is(err, agent.ErrApprovalClosed),
		errors.Is(err, agent.ErrDuplicateGrant),
		errors.Is(err, agent.ErrApprovalExpired),
		errors.Is(err, agent.ErrNotAwaiting),
		errors.Is(err, lake.ErrBreakClosed):
		writeError(w, r, http.StatusConflict, CodeConflict, err.Error())
	default:
		var apiErr Error
		if errors.As(err, &apiErr) {
			writeError(w, r, statusForCode(apiErr.Code), apiErr.Code, apiErr.Message)
			return
		}
		writeError(w, r, http.StatusInternalServerError, CodeInternal, err.Error())
	}
}

func statusForCode(code string) int {
	switch code {
	case CodeBadRequest:
		return http.StatusBadRequest
	case CodeUnauthenticated:
		return http.StatusUnauthorized
	case CodeForbidden:
		return http.StatusForbidden
	case CodeNotFound:
		return http.StatusNotFound
	case CodeConflict:
		return http.StatusConflict
	case CodeTooLarge:
		return http.StatusRequestEntityTooLarge
	case CodeRateLimited:
		return http.StatusTooManyRequests
	default:
		return http.StatusInternalServerError
	}
}

// decodeJSON reads a JSON body into v. An empty body is accepted as the zero
// value, because several endpoints take an optional note and requiring "{}" of
// a caller is friction with no security value.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return Error{Code: CodeTooLarge, Message: fmt.Sprintf("request body exceeds %d bytes", tooLarge.Limit)}
		}
		if errors.Is(err, http.ErrBodyReadAfterClose) {
			return Error{Code: CodeBadRequest, Message: "request body was closed"}
		}
		if err.Error() == "EOF" {
			return nil
		}
		return Error{Code: CodeBadRequest, Message: "malformed JSON body: " + err.Error()}
	}
	return nil
}

// intQuery reads a bounded integer query parameter.
func intQuery(r *http.Request, name string, def, min, max int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}
