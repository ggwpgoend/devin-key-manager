package devin_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ggwpgoend/devin-key-manager/internal/devin"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) (*devin.Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := devin.NewClient("test-token", devin.WithBaseURL(srv.URL))
	return c, srv
}

func TestCreateSessionSendsBearerAndBody(t *testing.T) {
	var seenAuth, seenUA, seenCT string
	var seenBody devin.CreateSessionRequest
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenUA = r.Header.Get("User-Agent")
		seenCT = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&seenBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"session_id":"devin-abc","url":"https://app.devin.ai/sessions/devin-abc"}`)
	})
	resp, err := c.CreateSession(context.Background(), devin.CreateSessionRequest{
		Prompt: "hello",
		Title:  "t",
		Tags:   []string{"a"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.SessionID != "devin-abc" {
		t.Fatalf("session id: %q", resp.SessionID)
	}
	if seenAuth != "Bearer test-token" {
		t.Errorf("auth header: %q", seenAuth)
	}
	if !strings.HasPrefix(seenUA, "devin-key-manager/") {
		t.Errorf("user-agent: %q", seenUA)
	}
	if seenCT != "application/json" {
		t.Errorf("content-type: %q", seenCT)
	}
	if seenBody.Prompt != "hello" || seenBody.Title != "t" || len(seenBody.Tags) != 1 {
		t.Errorf("body roundtrip: %+v", seenBody)
	}
}

func TestGetSessionDecodesMessages(t *testing.T) {
	const fixture = `{
		"session_id": "devin-x",
		"status": "running",
		"messages": [
			{"type":"user_message","message":"hi","timestamp":"2026-05-13T10:00:00Z"},
			{"type":"devin_message","message":"hello","timestamp":"2026-05-13T10:00:05Z"}
		]
	}`
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/session/devin-x" {
			t.Errorf("path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fixture)
	})
	sess, err := c.GetSession(context.Background(), "devin-x")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if sess.Status != "running" {
		t.Errorf("status: %q", sess.Status)
	}
	if len(sess.Messages) != 2 {
		t.Fatalf("messages: %d", len(sess.Messages))
	}
	if sess.Messages[0].Type != "user_message" || sess.Messages[1].Message != "hello" {
		t.Errorf("messages: %+v", sess.Messages)
	}
	if sess.Messages[0].Timestamp.IsZero() || sess.Messages[0].Timestamp.Year() != 2026 {
		t.Errorf("timestamp parse: %v", sess.Messages[0].Timestamp)
	}
}

func TestSendMessage(t *testing.T) {
	var seenPath string
	var seenBody devin.SendMessageRequest
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&seenBody)
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.SendMessage(context.Background(), "devin-z", "follow-up"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if seenPath != "/session/devin-z/message" {
		t.Errorf("path: %q", seenPath)
	}
	if seenBody.Message != "follow-up" {
		t.Errorf("body: %+v", seenBody)
	}
}

func TestSendMessageRejectsEmpty(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be hit")
	})
	if err := c.SendMessage(context.Background(), "devin-a", "  "); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestMapStatusQuota(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"402 plain", http.StatusPaymentRequired, "go away", devin.ErrQuotaExhausted},
		{"429 with quota text", http.StatusTooManyRequests, `{"error":"daily ACUs quota reached"}`, devin.ErrQuotaExhausted},
		{"401", http.StatusUnauthorized, "", devin.ErrUnauthorized},
		{"403", http.StatusForbidden, "", devin.ErrUnauthorized},
		{"404", http.StatusNotFound, "", devin.ErrNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			_, err := c.CreateSession(context.Background(), devin.CreateSessionRequest{Prompt: "x"})
			if !errors.Is(err, tc.want) {
				t.Errorf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestMapStatusRateLimitNotQuota(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, "slow down")
	})
	_, err := c.CreateSession(context.Background(), devin.CreateSessionRequest{Prompt: "x"})
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, devin.ErrQuotaExhausted) {
		t.Fatalf("429 without quota markers should NOT map to ErrQuotaExhausted: %v", err)
	}
	var apiErr *devin.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("want APIError 429, got %v", err)
	}
}

func TestUploadAttachment(t *testing.T) {
	var seenCT, seenFilename, seenContents string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seenCT = r.Header.Get("Content-Type")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		f, h, err := r.FormFile("file")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		seenFilename = h.Filename
		data, _ := io.ReadAll(f)
		seenContents = string(data)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"attachment_url":"https://files.devin.ai/abc"}`)
	})
	url, err := c.UploadAttachment(context.Background(), "handoff.md", strings.NewReader("# handoff"))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if url != "https://files.devin.ai/abc" {
		t.Errorf("url: %q", url)
	}
	if !strings.HasPrefix(seenCT, "multipart/form-data") {
		t.Errorf("content-type: %q", seenCT)
	}
	if seenFilename != "handoff.md" || seenContents != "# handoff" {
		t.Errorf("file: %q %q", seenFilename, seenContents)
	}
}

func TestRespectsContextCancel(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := c.GetSession(ctx, "x"); err == nil {
		t.Fatal("expected deadline error")
	}
}
