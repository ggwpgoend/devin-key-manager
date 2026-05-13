package devin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is the production Devin Cloud REST API root.
const DefaultBaseURL = "https://api.devin.ai/v1"

// Client talks to the Devin Cloud REST API on behalf of a single API key.
// Construct one per managed key; do not share across keys because each
// instance bakes a Bearer token into outgoing requests.
type Client struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
	userAgent  string
}

// Option mutates the client during construction. Use With* helpers below to
// override defaults like the base URL or HTTP timeout.
type Option func(*Client)

// WithBaseURL overrides the API root. Useful for tests against an httptest
// server.
func WithBaseURL(u string) Option {
	return func(c *Client) {
		if u != "" {
			c.baseURL = strings.TrimRight(u, "/")
		}
	}
}

// WithHTTPClient swaps the underlying HTTP client (e.g. with a custom
// transport for retries / logging).
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.httpClient = h
		}
	}
}

// WithUserAgent sets the User-Agent header. Default is "devin-key-manager/<ver>".
func WithUserAgent(ua string) Option {
	return func(c *Client) {
		if ua != "" {
			c.userAgent = ua
		}
	}
}

// NewClient returns a Client primed with apiKey. The default HTTP timeout is
// 60 seconds — generous, because session creation can be slow.
func NewClient(apiKey string, opts ...Option) *Client {
	c := &Client{
		apiKey:     apiKey,
		baseURL:    DefaultBaseURL,
		httpClient: &http.Client{Timeout: 60 * time.Second},
		userAgent:  "devin-key-manager/dev",
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// CreateSession asks Devin to start a new session with the given prompt. The
// returned SessionID is the canonical handle for all subsequent operations.
func (c *Client) CreateSession(ctx context.Context, req CreateSessionRequest) (CreateSessionResponse, error) {
	var out CreateSessionResponse
	if err := c.do(ctx, http.MethodPost, "/sessions", req, &out); err != nil {
		return CreateSessionResponse{}, err
	}
	return out, nil
}

// GetSession fetches the current state and message history of a session.
func (c *Client) GetSession(ctx context.Context, sessionID string) (Session, error) {
	if sessionID == "" {
		return Session{}, fmt.Errorf("devin: GetSession: empty session id")
	}
	var out Session
	if err := c.do(ctx, http.MethodGet, "/session/"+url.PathEscape(sessionID), nil, &out); err != nil {
		return Session{}, err
	}
	if out.ID == "" {
		out.ID = sessionID
	}
	return out, nil
}

// SendMessage appends a message to an existing session. Returns once the API
// acknowledges receipt; Devin processes the message asynchronously.
func (c *Client) SendMessage(ctx context.Context, sessionID, text string) error {
	if sessionID == "" {
		return fmt.Errorf("devin: SendMessage: empty session id")
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("devin: SendMessage: empty body")
	}
	return c.do(ctx, http.MethodPost, "/session/"+url.PathEscape(sessionID)+"/message", SendMessageRequest{Message: text}, nil)
}

// Validate performs a lightweight authenticated probe of the Devin API to
// determine whether the configured key is healthy. It calls GET /v1/sessions
// with limit=1 (the cheapest authenticated endpoint) and classifies the result
// into one of the ValidateStatus constants.
//
// Unlike the rest of the client, Validate does not return a Go error — it
// returns a populated ValidateResult for every outcome (success, auth failure,
// quota exhaustion, transport error, etc.) so callers can switch on Status
// without unwrapping.
func (c *Client) Validate(ctx context.Context) ValidateResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/sessions?limit=1", nil)
	if err != nil {
		return ValidateResult{Status: ValidateNetworkError, Error: err.Error()}
	}
	req.Header.Set("Accept", "application/json")
	c.setCommonHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ValidateResult{Status: ValidateNetworkError, Error: err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return classifyValidate(resp.StatusCode, body)
}

func classifyValidate(code int, body []byte) ValidateResult {
	switch {
	case code >= 200 && code < 300:
		return ValidateResult{Status: ValidateValid, HTTPStatus: code}
	case code == http.StatusPaymentRequired:
		return ValidateResult{Status: ValidateQuotaExhausted, HTTPStatus: code, Error: "quota exhausted (HTTP 402)"}
	case code == http.StatusTooManyRequests:
		if looksLikeQuota(body) {
			return ValidateResult{Status: ValidateQuotaExhausted, HTTPStatus: code, Error: "quota exhausted (HTTP 429)"}
		}
		return ValidateResult{Status: ValidateRateLimited, HTTPStatus: code, Error: "rate limited (HTTP 429)"}
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return ValidateResult{Status: ValidateUnauthorized, HTTPStatus: code, Error: "unauthorized (HTTP " + fmt.Sprint(code) + ")"}
	default:
		return ValidateResult{
			Status:     ValidateAPIError,
			HTTPStatus: code,
			Error:      "api error " + httpStatusText(code) + ": " + truncate(string(body), 200),
		}
	}
}

// UploadAttachment posts a file to /v1/attachments and returns the URL Devin
// expects to receive in subsequent prompts. The Reader is consumed eagerly.
func (c *Client) UploadAttachment(ctx context.Context, filename string, body io.Reader) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return "", fmt.Errorf("devin: form file: %w", err)
	}
	if _, err := io.Copy(part, body); err != nil {
		return "", fmt.Errorf("devin: copy body: %w", err)
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("devin: close multipart: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/attachments", &buf)
	if err != nil {
		return "", fmt.Errorf("devin: build request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	c.setCommonHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("devin: send: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if err := mapStatus(resp.StatusCode, raw); err != nil {
		return "", err
	}
	var out AttachmentResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("devin: decode attachment: %w", err)
	}
	return out.AttachmentURL, nil
}

// do is the common request/response plumbing. Pass nil for in when there's no
// body and nil for out when the caller does not care about the response shape.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("devin: marshal: %w", err)
		}
		body = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("devin: build request: %w", err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	c.setCommonHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("devin: send: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if err := mapStatus(resp.StatusCode, raw); err != nil {
		return err
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("devin: decode: %w (body=%s)", err, truncate(string(raw), 256))
	}
	return nil
}

func (c *Client) setCommonHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("User-Agent", c.userAgent)
}

// mapStatus converts an HTTP status code into the appropriate sentinel error.
// Devin Cloud surfaces ACU exhaustion via 402 (and some plans via 429 with a
// "quota" hint in the body), so we cover both.
func mapStatus(code int, body []byte) error {
	switch {
	case code >= 200 && code < 300:
		return nil
	case code == http.StatusPaymentRequired:
		return ErrQuotaExhausted
	case code == http.StatusTooManyRequests:
		if looksLikeQuota(body) {
			return ErrQuotaExhausted
		}
		return &APIError{StatusCode: code, Body: string(body)}
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return ErrUnauthorized
	case code == http.StatusNotFound:
		return ErrNotFound
	default:
		return &APIError{StatusCode: code, Body: string(body)}
	}
}

func looksLikeQuota(body []byte) bool {
	low := strings.ToLower(string(body))
	for _, hint := range []string{"quota", "acus", "limit reached", "limit exceeded", "no acus"} {
		if strings.Contains(low, hint) {
			return true
		}
	}
	return false
}

func httpStatusText(code int) string {
	if t := http.StatusText(code); t != "" {
		return fmt.Sprintf("%d %s", code, t)
	}
	return fmt.Sprintf("%d", code)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
