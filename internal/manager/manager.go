// Package manager is the orchestration layer that ties keys, tasks, sessions
// and the Devin API client together.
//
// External callers (HTTP handlers, the background poller) interact only with
// the Manager type — they never talk to Devin directly. This keeps the
// "which key am I using right now?" decision in a single place and makes the
// quota-detection + handoff flow (PR-3) easy to slot in.
package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ggwpgoend/devin-key-manager/internal/devin"
	"github.com/ggwpgoend/devin-key-manager/internal/handoffs"
	"github.com/ggwpgoend/devin-key-manager/internal/keys"
	"github.com/ggwpgoend/devin-key-manager/internal/sessions"
	"github.com/ggwpgoend/devin-key-manager/internal/tasks"
)

// ClientFactory builds a Devin API client from a plaintext API key. Injected
// so tests can swap in an httptest-backed client without hitting the real
// service.
type ClientFactory func(apiKey string) *devin.Client

// DefaultClientFactory returns a production Devin client. Manager uses this
// when no factory is provided.
func DefaultClientFactory(apiKey string) *devin.Client {
	return devin.NewClient(apiKey)
}

// Manager wires the application-level workflows.
type Manager struct {
	logger   *slog.Logger
	keys     *keys.Repo
	tasks    *tasks.Repo
	sessions *sessions.Repo
	handoffs *handoffs.Repo
	clientOf ClientFactory
	now      func() time.Time
}

// Options bundles optional Manager configuration.
type Options struct {
	Logger        *slog.Logger
	ClientFactory ClientFactory
	Now           func() time.Time
}

// New constructs a Manager. Pass a non-nil handoffs.Repo so the quota
// rotation flow can persist handoff markdown; production wires this through
// from main.go.
func New(k *keys.Repo, t *tasks.Repo, s *sessions.Repo, h *handoffs.Repo, opts Options) *Manager {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	factory := opts.ClientFactory
	if factory == nil {
		factory = DefaultClientFactory
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Manager{
		logger:   logger,
		keys:     k,
		tasks:    t,
		sessions: s,
		handoffs: h,
		clientOf: factory,
		now:      now,
	}
}

// StartTaskInput captures the user-supplied fields for StartTask.
type StartTaskInput struct {
	Title  string
	Prompt string
}

// StartTaskResult is the outcome of StartTask.
type StartTaskResult struct {
	Task    tasks.Task
	Session sessions.Session
	Key     keys.Key
}

// ErrNoActiveKey is re-exported so HTTP handlers don't have to import the keys
// package just to check this error.
var ErrNoActiveKey = keys.ErrNoActiveKey

// StartTask is the top-level "submit prompt" workflow. It picks an active key,
// creates a Devin session, persists the local task/session rows, seeds the
// local conversation cache with the user prompt, and returns everything the
// UI needs to redirect to the chat view.
//
// If anything fails after the local task is inserted, the task is left in
// state 'failed' and the error is returned. The caller does not need to clean
// up partial state.
func (m *Manager) StartTask(ctx context.Context, in StartTaskInput) (StartTaskResult, error) {
	in.Title = strings.TrimSpace(in.Title)
	in.Prompt = strings.TrimSpace(in.Prompt)
	if in.Prompt == "" {
		return StartTaskResult{}, errors.New("manager: prompt is required")
	}
	if in.Title == "" {
		in.Title = deriveTitle(in.Prompt)
	}

	key, err := m.keys.Pick(ctx)
	if err != nil {
		return StartTaskResult{}, err
	}

	task, err := m.tasks.Create(ctx, tasks.CreateInput{Title: in.Title, Prompt: in.Prompt})
	if err != nil {
		return StartTaskResult{}, fmt.Errorf("manager: create task: %w", err)
	}

	sess, err := m.sessions.Create(ctx, sessions.CreateInput{TaskID: task.ID, KeyID: key.ID})
	if err != nil {
		_ = m.tasks.SetStatus(ctx, task.ID, tasks.StatusFailed)
		return StartTaskResult{}, fmt.Errorf("manager: create local session: %w", err)
	}

	plaintext, err := m.keys.Reveal(ctx, key.ID)
	if err != nil {
		_ = m.sessions.SetStatus(ctx, sess.ID, sessions.StatusFailed, "reveal key: "+err.Error())
		_ = m.tasks.SetStatus(ctx, task.ID, tasks.StatusFailed)
		return StartTaskResult{}, fmt.Errorf("manager: reveal key: %w", err)
	}

	client := m.clientOf(plaintext)
	resp, err := client.CreateSession(ctx, devin.CreateSessionRequest{
		Prompt: in.Prompt,
		Title:  in.Title,
	})
	if err != nil {
		reason := "create devin session: " + err.Error()
		status := sessions.StatusFailed
		if errors.Is(err, devin.ErrQuotaExhausted) {
			status = sessions.StatusQuotaExhausted
			// First-try quota: cooldown the offending key and rotate to a
			// fresh one. Because no conversation has happened yet, the
			// "handoff" is just the original prompt — the user shouldn't
			// have to retry by hand.
			_ = m.sessions.SetStatus(ctx, sess.ID, status, reason)
			_ = m.tasks.SetStatus(ctx, task.ID, tasks.StatusRunning)
			rotated, rerr := m.rotateOnQuota(ctx, sess, "create_session quota_exhausted")
			if rerr == nil {
				return StartTaskResult{Task: rotated.Task, Session: rotated.ToSession, Key: rotated.NewKey}, nil
			}
			if !errors.Is(rerr, keys.ErrNoActiveKey) {
				m.logger.Warn("start task: rotate after first-try quota failed", "err", rerr)
			}
			_ = m.tasks.SetStatus(ctx, task.ID, tasks.StatusPaused)
			return StartTaskResult{}, err
		}
		_ = m.sessions.SetStatus(ctx, sess.ID, status, reason)
		_ = m.tasks.SetStatus(ctx, task.ID, tasks.StatusFailed)
		return StartTaskResult{}, err
	}

	if err := m.sessions.AttachDevinSessionID(ctx, sess.ID, resp.SessionID); err != nil {
		return StartTaskResult{}, fmt.Errorf("manager: attach devin session id: %w", err)
	}
	if _, err := m.sessions.AppendMessage(ctx, sess.ID, sessions.RoleUser, in.Prompt, m.now().UTC()); err != nil {
		return StartTaskResult{}, fmt.Errorf("manager: seed user message: %w", err)
	}
	if err := m.tasks.SetStatus(ctx, task.ID, tasks.StatusRunning); err != nil {
		return StartTaskResult{}, fmt.Errorf("manager: set task running: %w", err)
	}
	if err := m.keys.MarkUsed(ctx, key.ID); err != nil {
		m.logger.Warn("mark used", "key_id", key.ID, "err", err)
	}

	sess, err = m.sessions.Get(ctx, sess.ID)
	if err != nil {
		return StartTaskResult{}, fmt.Errorf("manager: reload session: %w", err)
	}
	task, err = m.tasks.Get(ctx, task.ID)
	if err != nil {
		return StartTaskResult{}, fmt.Errorf("manager: reload task: %w", err)
	}
	return StartTaskResult{Task: task, Session: sess, Key: key}, nil
}

// SendFollowUp posts a follow-up user message to an existing session, mirrors
// it into the local cache so the UI shows it immediately, and bumps the task
// updated_at so it sorts to the top of the list.
func (m *Manager) SendFollowUp(ctx context.Context, sessionID, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("manager: follow-up text is required")
	}
	sess, err := m.sessions.Get(ctx, sessionID)
	if err != nil {
		return err
	}
	if sess.DevinSessionID == "" {
		return errors.New("manager: session has no devin session id yet")
	}
	plaintext, err := m.keys.Reveal(ctx, sess.KeyID)
	if err != nil {
		return fmt.Errorf("manager: reveal key: %w", err)
	}
	client := m.clientOf(plaintext)
	if err := client.SendMessage(ctx, sess.DevinSessionID, text); err != nil {
		if errors.Is(err, devin.ErrQuotaExhausted) {
			// Cache the pending user message locally so the handoff prompt
			// includes it — Devin needs to see what the user just tried to
			// send.
			if _, aerr := m.sessions.AppendMessage(ctx, sessionID, sessions.RoleUser, text, m.now().UTC()); aerr != nil {
				m.logger.Warn("manager: cache pending message before rotate", "err", aerr)
			}
			if _, rerr := m.rotateOnQuota(ctx, sess, "send_message quota_exhausted"); rerr != nil {
				m.logger.Warn("manager: rotate after send_message quota failed", "session_id", sessionID, "err", rerr)
			}
		}
		return err
	}
	if _, err := m.sessions.AppendMessage(ctx, sessionID, sessions.RoleUser, text, m.now().UTC()); err != nil {
		return fmt.Errorf("manager: append follow-up: %w", err)
	}
	_ = m.tasks.Touch(ctx, sess.TaskID)
	_ = m.keys.MarkUsed(ctx, sess.KeyID)
	return nil
}

// SyncSession pulls the latest state of a session from Devin, replaces the
// cached message stream, and updates local status. Safe to call concurrently
// for different sessions; concurrent calls for the same session are serialised
// by SQLite's WAL writer.
func (m *Manager) SyncSession(ctx context.Context, sessionID string) error {
	sess, err := m.sessions.Get(ctx, sessionID)
	if err != nil {
		return err
	}
	if sess.DevinSessionID == "" {
		return nil // nothing to poll yet
	}
	plaintext, err := m.keys.Reveal(ctx, sess.KeyID)
	if err != nil {
		return fmt.Errorf("manager: reveal key: %w", err)
	}
	client := m.clientOf(plaintext)
	remote, err := client.GetSession(ctx, sess.DevinSessionID)
	if err != nil {
		if errors.Is(err, devin.ErrQuotaExhausted) {
			if _, rerr := m.rotateOnQuota(ctx, sess, "poll: quota exhausted"); rerr != nil {
				m.logger.Warn("manager: rotate after poll quota failed", "session_id", sessionID, "err", rerr)
			}
		}
		return err
	}
	cached := make([]sessions.Message, 0, len(remote.Messages))
	for _, msg := range remote.Messages {
		cached = append(cached, sessions.Message{
			SessionID: sessionID,
			Role:      roleFromDevinType(msg.Type),
			Content:   msg.Message,
			Timestamp: msg.Timestamp,
		})
	}
	if err := m.sessions.ReplaceMessages(ctx, sessionID, cached); err != nil {
		return err
	}
	if status := statusFromDevin(remote.Status); status != "" {
		_ = m.sessions.SetStatus(ctx, sessionID, status, remote.Status)
	}
	_ = m.sessions.MarkPolled(ctx, sessionID)
	_ = m.tasks.Touch(ctx, sess.TaskID)
	return nil
}

// roleFromDevinType maps a Devin message-type string to a local Role.
// Unrecognised types fall back to "system" so the UI can still display them.
func roleFromDevinType(t string) sessions.Role {
	switch strings.ToLower(t) {
	case "user_message":
		return sessions.RoleUser
	case "devin_message", "agent_message", "assistant_message":
		return sessions.RoleAssistant
	default:
		return sessions.RoleSystem
	}
}

// statusFromDevin maps a remote status string to our local Status enum.
// Returns "" when the remote status doesn't correspond to a terminal state we
// track (so the local status remains untouched).
func statusFromDevin(s string) sessions.Status {
	switch strings.ToLower(s) {
	case "running", "in_progress", "working":
		return sessions.StatusRunning
	case "blocked", "waiting_for_user":
		return sessions.StatusBlocked
	case "completed", "finished":
		return sessions.StatusCompleted
	case "failed", "errored":
		return sessions.StatusFailed
	default:
		return ""
	}
}

// deriveTitle returns a short title from the first line of the prompt.
func deriveTitle(prompt string) string {
	for _, line := range strings.Split(prompt, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > 80 {
			line = line[:80] + "…"
		}
		return line
	}
	return "Untitled task"
}
