package manager_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ggwpgoend/devin-key-manager/internal/keys"
	"github.com/ggwpgoend/devin-key-manager/internal/manager"
	"github.com/ggwpgoend/devin-key-manager/internal/sessions"
)

// startSimpleTask creates a key and a running session so resume tests
// have something to diagnose.
func startSimpleTask(t *testing.T, f *fixtures) sessions.Session {
	t.Helper()
	_, err := f.keys.Create(context.Background(), keys.CreateInput{
		Label: "trial", Plan: keys.PlanTrial, APIKey: "sk-trial",
	})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	f.handler.HandleFunc("/sessions", func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"session_id":"devin-resume","url":"https://app.devin.ai/sessions/devin-resume"}`)
	})
	res, err := f.mgr.StartTask(context.Background(), manager.StartTaskInput{
		Title: "t", Prompt: "p",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	return res.Session
}

func TestDiagnose_HealthyRunningSession(t *testing.T) {
	f := newFixtures(t)
	sess := startSimpleTask(t, f)
	if err := f.sessions.SetStatus(context.Background(), sess.ID, sessions.StatusRunning, ""); err != nil {
		t.Fatalf("set status: %v", err)
	}
	// Append a fresh assistant message so idle detection is satisfied.
	if _, err := f.sessions.AppendMessage(context.Background(), sess.ID, sessions.RoleAssistant, "hi", time.Now().UTC()); err != nil {
		t.Fatalf("append msg: %v", err)
	}
	d, err := f.mgr.DiagnoseSession(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("diagnose: %v", err)
	}
	if d.Action != manager.ActionNone {
		t.Fatalf("expected ActionNone for healthy session, got %s (%s)", d.Action, d.Detail)
	}
}

func TestDiagnose_TerminalSessionNoAction(t *testing.T) {
	f := newFixtures(t)
	sess := startSimpleTask(t, f)
	if err := f.sessions.SetStatus(context.Background(), sess.ID, sessions.StatusCompleted, "done"); err != nil {
		t.Fatalf("set status: %v", err)
	}
	d, err := f.mgr.DiagnoseSession(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("diagnose: %v", err)
	}
	if d.Reason != manager.ResumeReasonTerminal || d.Action != manager.ActionNone {
		t.Fatalf("expected terminal/none, got %s/%s", d.Reason, d.Action)
	}
}

func TestDiagnose_FailedSessionRecommendsHandoff(t *testing.T) {
	f := newFixtures(t)
	sess := startSimpleTask(t, f)
	if err := f.sessions.SetStatus(context.Background(), sess.ID, sessions.StatusFailed, "boom"); err != nil {
		t.Fatalf("set status: %v", err)
	}
	d, err := f.mgr.DiagnoseSession(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("diagnose: %v", err)
	}
	if d.Reason != manager.ResumeReasonFailed || d.Action != manager.ActionHandoff {
		t.Fatalf("expected failed/handoff, got %s/%s", d.Reason, d.Action)
	}
	if !strings.Contains(d.Detail, "boom") {
		t.Fatalf("expected EndReason in detail; got %q", d.Detail)
	}
}

func TestDiagnose_QuotaExhaustedRecommendsHandoff(t *testing.T) {
	f := newFixtures(t)
	sess := startSimpleTask(t, f)
	if err := f.sessions.SetStatus(context.Background(), sess.ID, sessions.StatusQuotaExhausted, ""); err != nil {
		t.Fatalf("set status: %v", err)
	}
	d, err := f.mgr.DiagnoseSession(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("diagnose: %v", err)
	}
	if d.Action != manager.ActionHandoff {
		t.Fatalf("expected handoff, got %s", d.Action)
	}
}
