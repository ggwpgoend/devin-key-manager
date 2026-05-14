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

	"github.com/ggwpgoend/devin-key-manager/internal/artifacts"
	"github.com/ggwpgoend/devin-key-manager/internal/devin"
	"github.com/ggwpgoend/devin-key-manager/internal/events"
	"github.com/ggwpgoend/devin-key-manager/internal/handoffs"
	"github.com/ggwpgoend/devin-key-manager/internal/keys"
	"github.com/ggwpgoend/devin-key-manager/internal/notifications"
	"github.com/ggwpgoend/devin-key-manager/internal/schedules"
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
	logger     *slog.Logger
	keys       *keys.Repo
	tasks      *tasks.Repo
	sessions   *sessions.Repo
	handoffs   *handoffs.Repo
	artifacts  *artifacts.Repo
	downloader *artifacts.Downloader
	notifs     *notifications.Repo
	bus        *events.Bus
	clientOf   ClientFactory
	now        func() time.Time
}

// Options bundles optional Manager configuration.
type Options struct {
	Logger        *slog.Logger
	ClientFactory ClientFactory
	Now           func() time.Time
	Artifacts     *artifacts.Repo
	Downloader    *artifacts.Downloader
	Notifications *notifications.Repo
	// Bus is an optional in-process event broker. When non-nil, the
	// manager publishes state changes (key/session/task/artifact/handoff)
	// so the web layer can fan them out over SSE for live UI updates.
	Bus *events.Bus
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
		logger:     logger,
		keys:       k,
		tasks:      t,
		sessions:   s,
		handoffs:   h,
		artifacts:  opts.Artifacts,
		downloader: opts.Downloader,
		notifs:     opts.Notifications,
		bus:        opts.Bus,
		clientOf:   factory,
		now:        now,
	}
}

// SetBus attaches an event bus after construction. Used in tests and in
// scenarios where the bus is constructed after the manager.
func (m *Manager) SetBus(b *events.Bus) { m.bus = b }

// publish is a nil-safe helper around bus.Publish so handler code stays
// terse: every state-change path can `m.publish(events.KindXxx, …)`
// without caring whether the bus was wired.
func (m *Manager) publish(kind events.Kind, data map[string]any) {
	if m == nil || m.bus == nil {
		return
	}
	m.bus.Publish(kind, data)
}

// SetNotifications attaches the notification event store after construction.
// Optional — when nil the manager simply skips appending events.
func (m *Manager) SetNotifications(n *notifications.Repo) { m.notifs = n }

// StartTaskInput captures the user-supplied fields for StartTask.
type StartTaskInput struct {
	Title  string
	Prompt string
	// PreferKeyID, when non-empty, asks the picker to favour this key
	// over the round-robin default. Falls back transparently if the key
	// is no longer active. Drives session-to-key stickiness (A9).
	PreferKeyID string
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

// ErrPromptRequired is returned by StartTask when the caller supplied no
// non-whitespace prompt. HTTP handlers translate this into a user-facing
// validation message rather than leaking the raw "manager: ..." string.
var ErrPromptRequired = errors.New("prompt is required")

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
		return StartTaskResult{}, ErrPromptRequired
	}
	if in.Title == "" {
		in.Title = deriveTitle(in.Prompt)
	}

	var (
		key keys.Key
		err error
	)
	if strings.TrimSpace(in.PreferKeyID) != "" {
		key, err = m.keys.PickWithPreference(ctx, in.PreferKeyID)
	} else {
		key, err = m.keys.Pick(ctx)
	}
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
	_ = m.keys.BumpSessionsCount(ctx, key.ID)

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
		_ = m.keys.RecordError(ctx, key.ID, err.Error())
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
	// Snapshot the previous Devin-side message count so we can detect new
	// assistant replies after the replace and surface them as browser
	// notifications.
	prevCached, _ := m.sessions.ListMessages(ctx, sessionID)
	prevDevin := countAssistantMessages(prevCached)

	cached := make([]sessions.Message, 0, len(remote.Messages))
	for _, msg := range remote.Messages {
		cached = append(cached, sessions.Message{
			SessionID: sessionID,
			Role:      roleFromDevinType(msg.Type),
			Content:   stripAttachmentMarkers(msg.Message),
			Timestamp: msg.Timestamp,
		})
	}
	if err := m.sessions.ReplaceMessages(ctx, sessionID, cached); err != nil {
		return err
	}
	m.notifyNewDevinMessages(ctx, sess, cached, prevDevin)
	if status := statusFromDevin(remote.Status); status != "" {
		_ = m.sessions.SetStatus(ctx, sessionID, status, remote.Status)
	}
	_ = m.sessions.MarkPolled(ctx, sessionID)
	_ = m.tasks.Touch(ctx, sess.TaskID)

	// Scan assistant messages for attachment URLs and enqueue downloads.
	m.extractAndEnqueueArtifacts(ctx, sess, remote.Messages)

	return nil
}

// extractAndEnqueueArtifacts scans remote messages for HTTP(S) URLs and
// creates pending artifact rows for any that haven't been seen before. The
// downloader picks them up asynchronously.
func (m *Manager) extractAndEnqueueArtifacts(ctx context.Context, sess sessions.Session, msgs []devin.Message) {
	if m.artifacts == nil || m.downloader == nil {
		return
	}
	for _, msg := range msgs {
		if msg.Type != "devin_message" && msg.Type != "agent_message" && msg.Type != "assistant_message" {
			continue
		}
		urls := artifacts.ExtractURLs(msg.Message)
		for _, eu := range urls {
			a, err := m.artifacts.Create(ctx, artifacts.CreateInput{
				TaskID:    sess.TaskID,
				SessionID: sess.ID,
				Filename:  eu.Filename,
				RemoteURL: eu.URL,
				Source:    artifacts.SourceDevin,
			})
			if errors.Is(err, artifacts.ErrAlreadyExists) {
				continue
			}
			if err != nil {
				m.logger.Warn("artifacts: create row", "url", eu.URL, "err", err)
				continue
			}
			m.downloader.Enqueue(ctx, a)
		}
	}
}

// roleFromDevinType maps a Devin message-type string to a local Role.
// Unrecognised types fall back to "system" so the UI can still display them.
//
// Devin's API has emitted several aliases for the user side over time —
// notably "initial_user_message" for the seeded prompt that opens a session.
// Treating that as a system message hid the user's first turn from the UI,
// so all of these get folded into RoleUser.
func roleFromDevinType(t string) sessions.Role {
	switch strings.ToLower(t) {
	case "user_message", "initial_user_message", "user", "prompt":
		return sessions.RoleUser
	case "devin_message", "agent_message", "assistant_message", "assistant":
		return sessions.RoleAssistant
	default:
		return sessions.RoleSystem
	}
}

// stripAttachmentMarkers removes Devin's machine-readable ATTACHMENT:{...}
// envelope lines from message bodies before we surface them in the UI. The
// markers are an internal protocol used to enqueue artifact downloads (see
// artifacts.ExtractURLs); they have no value to a human reader and make chat
// bubbles look broken when the JSON leaks through.
//
// The implementation is intentionally conservative: we drop any *line* whose
// first non-whitespace token is "ATTACHMENT:{". Multi-line JSON envelopes
// would survive trimming today; if Devin ever starts emitting them we will
// extend this here rather than upstream.
func stripAttachmentMarkers(s string) string {
	if s == "" || !strings.Contains(s, "ATTACHMENT:{") {
		return s
	}
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "ATTACHMENT:{") {
			continue
		}
		out = append(out, ln)
	}
	// Collapse runs of blank lines that the strip introduced.
	joined := strings.Join(out, "\n")
	for strings.Contains(joined, "\n\n\n") {
		joined = strings.ReplaceAll(joined, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(joined)
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

// ArtifactsRepo exposes the artifacts repository so the web layer can query
// artifacts for rendering. Returns nil if artifacts support was not wired in.
func (m *Manager) ArtifactsRepo() *artifacts.Repo { return m.artifacts }

// SetDownloader injects the artifacts downloader after construction. This is
// necessary because the downloader needs a BearerProvider that calls back into
// the manager, which creates a chicken-and-egg ordering issue at startup.
func (m *Manager) SetDownloader(d *artifacts.Downloader) { m.downloader = d }

// ArtifactsRoot returns the on-disk root directory the downloader is using,
// or "" if artifacts support was not wired in. Used by the UI to render the
// per-session folder path and the "Open folder" action.
func (m *Manager) ArtifactsRoot() string {
	if m.downloader == nil {
		return ""
	}
	return m.downloader.Root()
}

// BearerForSession returns the decrypted API key associated with a session's
// key. Used as an artifacts.BearerProvider so the downloader can authenticate
// when fetching remote attachment URLs.
func (m *Manager) BearerForSession(ctx context.Context, sessionID string) (string, error) {
	sess, err := m.sessions.Get(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("manager: get session: %w", err)
	}
	plaintext, err := m.keys.Reveal(ctx, sess.KeyID)
	if err != nil {
		return "", fmt.Errorf("manager: reveal key: %w", err)
	}
	return plaintext, nil
}

// SnapDesktop sends a screenshot-request prompt to a running session's Devin
// instance. The reply (which should include an image attachment) will be picked
// up on the next SyncSession cycle and auto-downloaded by the artifact
// downloader. Returns the session ID the message was sent to.
func (m *Manager) SnapDesktop(ctx context.Context, sessionID string) error {
	const prompt = "Please take a screenshot of your current desktop and include it as an image attachment in your reply. Do not include any other commentary."
	return m.SendFollowUp(ctx, sessionID, prompt)
}

// StartScheduledTask is the Runner callback used by internal/scheduler. It
// kicks off a normal task using the schedule's title + prompt and returns
// the new Devin session id so the scheduler can pin it to last_session_id.
func (m *Manager) StartScheduledTask(ctx context.Context, sch schedules.Schedule) (string, error) {
	res, err := m.StartTask(ctx, StartTaskInput{
		Title:  sch.Title,
		Prompt: sch.Prompt,
	})
	if err != nil {
		return "", err
	}
	return res.Session.ID, nil
}

// countAssistantMessages returns the number of Devin-side messages in s.
func countAssistantMessages(msgs []sessions.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role == sessions.RoleAssistant {
			n++
		}
	}
	return n
}

// notifyNewDevinMessages appends a notification event when SyncSession sees
// new assistant replies. We summarise the last new message so the toast has
// useful content; if there are many, we just say how many arrived.
func (m *Manager) notifyNewDevinMessages(ctx context.Context, sess sessions.Session, cached []sessions.Message, prevCount int) {
	if m.notifs == nil {
		return
	}
	nowCount := countAssistantMessages(cached)
	if nowCount <= prevCount {
		return
	}
	// Find the newest assistant reply to use as the body.
	var lastBody string
	for i := len(cached) - 1; i >= 0; i-- {
		if cached[i].Role == sessions.RoleAssistant {
			lastBody = cached[i].Content
			break
		}
	}
	if r := []rune(lastBody); len(r) > 200 {
		// Slice by runes, not bytes — otherwise multi-byte characters
		// (Cyrillic, CJK, emoji) get cut mid-sequence and end up as
		// mojibake in the browser-side Notification body.
		lastBody = string(r[:200]) + "…"
	}
	title := "Devin replied"
	if delta := nowCount - prevCount; delta > 1 {
		title = fmt.Sprintf("Devin replied (%d new)", delta)
	}
	evID, err := m.notifs.Append(ctx, notifications.AppendInput{
		Kind:             notifications.KindDevinMessage,
		Title:            title,
		Body:             lastBody,
		URL:              "/sessions/" + sess.ID,
		RelatedTaskID:    sess.TaskID,
		RelatedSessionID: sess.ID,
	})
	if err != nil {
		m.logger.Warn("notify devin message", "err", err)
	}
	// Two SSE publishes: one as a typed state-change event (UI subscribers),
	// one as a generic notification (browser Notification toast).
	m.publish(events.KindSessionMessage, map[string]any{
		"session_id": sess.ID,
		"task_id":    sess.TaskID,
		"count":      nowCount,
		"delta":      nowCount - prevCount,
		"title":      title,
		"preview":    lastBody,
	})
	m.publish(events.KindNotification, map[string]any{
		"event_id": evID,
		"kind":     string(notifications.KindDevinMessage),
		"title":    title,
		"body":     lastBody,
		"url":      "/sessions/" + sess.ID,
	})
}

// NotifyHandoff appends a notification event for a quota-driven rotation.
// Called from the rotation code path.
func (m *Manager) NotifyHandoff(ctx context.Context, fromSess, toSess sessions.Session, reason string) {
	if m.notifs == nil {
		return
	}
	evID, err := m.notifs.Append(ctx, notifications.AppendInput{
		Kind:             notifications.KindHandoff,
		Title:            "Handoff to new key",
		Body:             reason,
		URL:              "/sessions/" + toSess.ID,
		RelatedTaskID:    fromSess.TaskID,
		RelatedSessionID: toSess.ID,
	})
	if err != nil {
		m.logger.Warn("notify handoff", "err", err)
	}
	m.publish(events.KindHandoffLinked, map[string]any{
		"task_id":         fromSess.TaskID,
		"from_session_id": fromSess.ID,
		"to_session_id":   toSess.ID,
		"reason":          reason,
	})
	m.publish(events.KindNotification, map[string]any{
		"event_id": evID,
		"kind":     string(notifications.KindHandoff),
		"title":    "Handoff to new key",
		"body":     reason,
		"url":      "/sessions/" + toSess.ID,
	})
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
