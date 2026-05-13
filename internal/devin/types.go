// Package devin is a thin HTTP client for the Devin Cloud REST API. It is
// intentionally minimal: each API call is one method on Client that returns
// strongly-typed structs, with explicit error types for quota / auth failures
// so callers can branch on them without sniffing strings.
package devin

import (
	"errors"
	"time"
)

// Session is the trimmed representation of a Devin session returned by
// /v1/session/{id}. Fields not used by this manager are deliberately omitted —
// json.Unmarshal silently ignores unknown keys.
type Session struct {
	ID               string    `json:"session_id"`
	Title            string    `json:"title,omitempty"`
	Status           string    `json:"status,omitempty"`
	URL              string    `json:"url,omitempty"`
	CreatedAt        time.Time `json:"created_at,omitempty"`
	UpdatedAt        time.Time `json:"updated_at,omitempty"`
	Messages         []Message `json:"messages,omitempty"`
	StructuredOutput any       `json:"structured_output,omitempty"`
}

// Message is one entry in a session conversation. The Devin API uses several
// type strings ("user_message", "devin_message", "system_message", etc.) — we
// preserve them verbatim and let the manager decide how to render each.
type Message struct {
	Type      string    `json:"type"`
	Message   string    `json:"message"`
	Username  string    `json:"username,omitempty"`
	Timestamp time.Time `json:"timestamp,omitempty"`
}

// CreateSessionRequest is the body sent to POST /v1/sessions.
type CreateSessionRequest struct {
	Prompt     string   `json:"prompt"`
	Title      string   `json:"title,omitempty"`
	PlaybookID string   `json:"playbook_id,omitempty"`
	SnapshotID string   `json:"snapshot_id,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Idempotent bool     `json:"idempotent,omitempty"`
}

// CreateSessionResponse mirrors the response from POST /v1/sessions.
type CreateSessionResponse struct {
	SessionID    string `json:"session_id"`
	URL          string `json:"url,omitempty"`
	IsNewSession bool   `json:"is_new_session,omitempty"`
}

// SendMessageRequest is the body sent to POST /v1/session/{id}/message.
type SendMessageRequest struct {
	Message string `json:"message"`
}

// AttachmentResponse mirrors the JSON body returned by /v1/attachments.
type AttachmentResponse struct {
	AttachmentURL string `json:"attachment_url"`
}

// Errors -------------------------------------------------------------------

// ErrQuotaExhausted signals that the API key has hit its ACU limit. The
// manager treats this as the trigger for cooldown + handoff. Mapped from
// HTTP 402 and from 429 with a quota-related body.
var ErrQuotaExhausted = errors.New("devin: quota exhausted")

// ErrUnauthorized signals that the API key is invalid or revoked.
var ErrUnauthorized = errors.New("devin: unauthorized")

// ErrNotFound signals a 404 response.
var ErrNotFound = errors.New("devin: not found")

// APIError wraps unexpected non-2xx responses for diagnostics.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return "devin: api error " + httpStatusText(e.StatusCode) + ": " + truncate(e.Body, 256)
}

// ValidateStatus is the machine-stable outcome tag returned by Client.Validate.
type ValidateStatus string

const (
	ValidateValid          ValidateStatus = "valid"
	ValidateUnauthorized   ValidateStatus = "unauthorized"
	ValidateQuotaExhausted ValidateStatus = "quota_exhausted"
	ValidateRateLimited    ValidateStatus = "rate_limited"
	ValidateNetworkError   ValidateStatus = "network_error"
	ValidateAPIError       ValidateStatus = "api_error"
)

// ValidateResult is the structured outcome of a key-validity check. Unlike the
// other client methods, Validate never returns a Go error: it always populates
// Status so callers can branch on it without sniffing error chains.
type ValidateResult struct {
	Status     ValidateStatus
	HTTPStatus int
	// Error is a human-readable description suitable for the dashboard.
	// Empty when Status == ValidateValid.
	Error string
}
