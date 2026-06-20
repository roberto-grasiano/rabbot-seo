package gsc

import (
	"errors"
	"fmt"
	"net/http"
)

// APIError is a typed googleapis error response. The Google JSON error envelope
// is {"error":{"code":N,"message":"...","status":"..."}}; we surface code,
// status, and message so callers can branch on quota vs permission vs transient.
type APIError struct {
	// HTTPStatus is the response status code (may differ from Code in odd cases).
	HTTPStatus int
	// Code is the numeric code from the error envelope (usually == HTTPStatus).
	Code int
	// Status is the canonical google.rpc.Code string, e.g. "PERMISSION_DENIED",
	// "RESOURCE_EXHAUSTED", "UNAUTHENTICATED".
	Status string
	// Message is the human-readable error message. It originates from Google and
	// never echoes the request's bearer/assertion.
	Message string
}

func (e *APIError) Error() string {
	if e.Status != "" {
		return fmt.Sprintf("gsc: api error %d (%s): %s", e.Code, e.Status, e.Message)
	}
	return fmt.Sprintf("gsc: api error %d: %s", e.Code, e.Message)
}

// IsQuotaExceeded reports whether this is a quota/rate-limit error — HTTP 429 or
// the RESOURCE_EXHAUSTED status. Callers use it to back off the per-day URL
// inspection budget without hammering.
func (e *APIError) IsQuotaExceeded() bool {
	return e.HTTPStatus == http.StatusTooManyRequests ||
		e.Code == http.StatusTooManyRequests ||
		e.Status == "RESOURCE_EXHAUSTED"
}

// IsRetryable reports whether err is a transient API error worth retrying:
// 429 (quota/rate) or any 5xx. A 4xx other than 429 (bad request, permission,
// auth) is permanent and not retried.
func IsRetryable(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.IsQuotaExceeded() {
		return true
	}
	return apiErr.HTTPStatus >= 500 && apiErr.HTTPStatus <= 599
}

// apiErrorEnvelope decodes the googleapis error JSON.
type apiErrorEnvelope struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}
