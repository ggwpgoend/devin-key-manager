package manager

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ggwpgoend/devin-key-manager/internal/devin"
	"github.com/ggwpgoend/devin-key-manager/internal/handoffs"
	"github.com/ggwpgoend/devin-key-manager/internal/keys"
	"github.com/ggwpgoend/devin-key-manager/internal/sessions"
	"github.com/ggwpgoend/devin-key-manager/internal/tasks"
)

// RotateReason explains why the manager kicked off a rotation. The reason is
// surfaced in handoff markdown and in the end_reason on the dying session so
// the user can trace what happened.
type RotateReason string

const (
	// ReasonQuotaExhausted means the active key returned 402/429-with-quota
	// while serving this session.
	ReasonQuotaExhausted RotateReason = "quota_exhausted"
	// ReasonManual means the user explicitly clicked "Rotate now" in the UI.
	ReasonManual RotateReason = "manual"
)

// RotateResult is the public outcome of a successful rotation. Callers
// (HTTP handlers, the poller) use it to log/redirect.
type RotateResult struct {
	Task          tasks.Task
	FromSession   sessions.Session
	ToSession     sessions.Session
	NewKey        keys.Key
	Handoff       handoffs.Handoff
	Reason        RotateReason
	CooldownUntil time.Time
}

// ErrAlreadyRotated guards against double-rotating the same dying session.
// A second rotate call on a session that is already in a terminal handoff
// state is a no-op from the caller's perspective.
var ErrAlreadyRotated = errors.New("manager: session already handed off")

// rotateOnQuota is the internal entry point used when an API call surfaces a
// quota signal. It marks the dying session quota_exhausted, puts the offending
// key on cooldown, generates a handoff markdown from the local message
// cache, picks the next active key, opens a fresh session with the handoff
// prompt, and links the two sessions via the handoffs table.
//
// Returns ErrNoActiveKey when no replacement key is currently available — in
// that case the dying session is still marked quota_exhausted and the task is
// flipped to 'paused' so the user can see the situation in the UI.
func (m *Manager) rotateOnQuota(ctx context.Context, dying sessions.Session, errReason string) (RotateResult, error) {
	return m.rotate(ctx, dying, ReasonQuotaExhausted, errReason)
}

// ForceRotate is the user-driven version of rotateOnQuota. It is safe to
// call on any non-terminal session: the dying session is marked
// handoff_pending (rather than quota_exhausted) and the offending key is not
// cooldowned. Useful when the user notices Devin is stuck or hallucinating
// and wants to start fresh on a different key without burning the current
// one.
func (m *Manager) ForceRotate(ctx context.Context, sessionID string) (RotateResult, error) {
	sess, err := m.sessions.Get(ctx, sessionID)
	if err != nil {
		return RotateResult{}, err
	}
	if !isRotatableSession(sess.Status) {
		return RotateResult{}, fmt.Errorf("manager: session %s is %s, cannot rotate", sessionID, sess.Status)
	}
	return m.rotate(ctx, sess, ReasonManual, "user requested rotate")
}

func isRotatableSession(s sessions.Status) bool {
	switch s {
	case sessions.StatusCreating, sessions.StatusRunning, sessions.StatusBlocked,
		sessions.StatusQuotaExhausted, sessions.StatusHandoffPending:
		return true
	default:
		return false
	}
}

func (m *Manager) rotate(ctx context.Context, dying sessions.Session, reason RotateReason, detail string) (RotateResult, error) {
	if dying.Status == sessions.StatusHandoffDone {
		return RotateResult{}, ErrAlreadyRotated
	}

	task, err := m.tasks.Get(ctx, dying.TaskID)
	if err != nil {
		return RotateResult{}, fmt.Errorf("manager: load task: %w", err)
	}

	// Stamp the dying session. Manual rotations use handoff_pending so the
	// poller stops fetching from the old key; quota rotations use
	// quota_exhausted so the key state and session state line up.
	dyingFinal := sessions.StatusQuotaExhausted
	if reason == ReasonManual {
		dyingFinal = sessions.StatusHandoffPending
	}
	endReason := fmt.Sprintf("%s: %s", reason, detail)
	if err := m.sessions.SetStatus(ctx, dying.ID, dyingFinal, endReason); err != nil {
		return RotateResult{}, fmt.Errorf("manager: stamp dying session: %w", err)
	}

	// Cool the offending key down for quota rotations. Manual rotations leave
	// the key alone — the user may just want to try a different one for
	// variety, not because the key is broken.
	var cooldownUntil time.Time
	if reason == ReasonQuotaExhausted {
		mark, err := m.keys.MarkQuotaExhausted(ctx, dying.KeyID)
		if err != nil {
			m.logger.Warn("rotate: mark quota", "key_id", dying.KeyID, "err", err)
		} else {
			cooldownUntil = mark.CooldownUntil
			m.logger.Info("rotate: cooldown applied",
				"key_id", dying.KeyID,
				"state", string(mark.NewState),
				"cooldown_until", mark.CooldownUntil,
				"cycles", mark.CyclesUsed,
			)
		}
	}

	// Build the handoff markdown from the local message cache plus the
	// original prompt. We never call Devin here — by the time we're in this
	// branch the dying key is already quota-exhausted and any extra API call
	// would just fail.
	msgs, err := m.sessions.ListMessages(ctx, dying.ID)
	if err != nil {
		return RotateResult{}, fmt.Errorf("manager: load messages: %w", err)
	}
	markdown := BuildHandoffMarkdown(task, dying, msgs, reason, detail, m.now().UTC())

	handoff, err := m.handoffs.Create(ctx, handoffs.CreateInput{
		TaskID:        task.ID,
		FromSessionID: dying.ID,
		Markdown:      markdown,
	})
	if err != nil {
		return RotateResult{}, fmt.Errorf("manager: create handoff: %w", err)
	}
	// Track whether we successfully linked the handoff to a new session.
	// Anything that returns before linkedOK = true triggers the deferred
	// cleanup so we don't leave orphaned rows in the handoffs table.
	// (#13: previously, if Pick / Reveal / CreateSession failed after the
	// handoff was inserted, the row stayed forever showing 'handed off to —'
	// in the task detail view.)
	linkedOK := false
	defer func() {
		if linkedOK {
			return
		}
		// Use a detached context so cleanup still runs when the caller's
		// context is the one that timed out / was cancelled.
		cleanCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if derr := m.handoffs.Delete(cleanCtx, handoff.ID); derr != nil {
			m.logger.Warn("rotate: cleanup orphan handoff failed",
				"handoff_id", handoff.ID, "err", derr)
		}
	}()

	// Pick the next active key, excluding the one we just cooldowned (Pick
	// already does this because the key is no longer in state=active).
	nextKey, err := m.keys.Pick(ctx)
	if err != nil {
		// No replacement key — pause the task and surface the error to the
		// caller. The orphan-handoff cleanup runs via defer above.
		if errors.Is(err, keys.ErrNoActiveKey) {
			_ = m.tasks.SetStatus(ctx, task.ID, tasks.StatusPaused)
		}
		return RotateResult{}, err
	}

	plaintext, err := m.keys.Reveal(ctx, nextKey.ID)
	if err != nil {
		return RotateResult{}, fmt.Errorf("manager: reveal next key: %w", err)
	}

	newLocal, err := m.sessions.Create(ctx, sessions.CreateInput{TaskID: task.ID, KeyID: nextKey.ID})
	if err != nil {
		return RotateResult{}, fmt.Errorf("manager: create replacement session: %w", err)
	}
	_ = m.keys.BumpSessionsCount(ctx, nextKey.ID)

	client := m.clientOf(plaintext)
	resp, err := client.CreateSession(ctx, devin.CreateSessionRequest{
		Prompt: markdown,
		Title:  task.Title + " (resumed)",
	})
	if err != nil {
		_ = m.keys.RecordError(ctx, nextKey.ID, err.Error())
		_ = m.sessions.SetStatus(ctx, newLocal.ID, sessions.StatusFailed, "rotate: create devin session: "+err.Error())
		// If the new key *also* hit quota, mark it cooldown too — but don't
		// recurse forever. The caller may decide to retry.
		if errors.Is(err, devin.ErrQuotaExhausted) {
			if _, qerr := m.keys.MarkQuotaExhausted(ctx, nextKey.ID); qerr != nil {
				m.logger.Warn("rotate: cooldown new key", "key_id", nextKey.ID, "err", qerr)
			}
		}
		return RotateResult{}, fmt.Errorf("manager: create devin session on new key: %w", err)
	}

	if err := m.sessions.AttachDevinSessionID(ctx, newLocal.ID, resp.SessionID); err != nil {
		return RotateResult{}, fmt.Errorf("manager: attach devin session id: %w", err)
	}
	if _, err := m.sessions.AppendMessage(ctx, newLocal.ID, sessions.RoleSystem,
		fmt.Sprintf("Resumed from previous session (%s). Reason: %s.", dying.ID, reason),
		m.now().UTC()); err != nil {
		m.logger.Warn("rotate: seed system message", "err", err)
	}
	if _, err := m.sessions.AppendMessage(ctx, newLocal.ID, sessions.RoleUser, markdown, m.now().UTC()); err != nil {
		m.logger.Warn("rotate: seed handoff message", "err", err)
	}

	if err := m.handoffs.LinkTo(ctx, handoff.ID, newLocal.ID); err != nil {
		m.logger.Warn("rotate: link handoff", "err", err)
	} else {
		// Successful link means the handoff row is no longer orphan-bait;
		// disable the deferred cleanup.
		linkedOK = true
	}
	if err := m.sessions.SetStatus(ctx, dying.ID, sessions.StatusHandoffDone,
		fmt.Sprintf("%s; rotated to %s", endReason, newLocal.ID)); err != nil {
		m.logger.Warn("rotate: mark handoff done", "err", err)
	}
	_ = m.keys.MarkUsed(ctx, nextKey.ID)
	_ = m.tasks.SetStatus(ctx, task.ID, tasks.StatusRunning)
	_ = m.tasks.Touch(ctx, task.ID)

	newSess, err := m.sessions.Get(ctx, newLocal.ID)
	if err != nil {
		return RotateResult{}, fmt.Errorf("manager: reload new session: %w", err)
	}
	handoffOut, _ := m.handoffs.GetForSession(ctx, newSess.ID)

	m.logger.Info("rotate done",
		"task_id", task.ID,
		"from_session_id", dying.ID,
		"to_session_id", newSess.ID,
		"new_key_id", nextKey.ID,
		"new_key_label", nextKey.Label,
		"reason", string(reason),
	)
	m.NotifyHandoff(ctx, dying, newSess, fmt.Sprintf("%s · new key: %s", reason, nextKey.Label))
	return RotateResult{
		Task:          task,
		FromSession:   dying,
		ToSession:     newSess,
		NewKey:        nextKey,
		Handoff:       handoffOut,
		Reason:        reason,
		CooldownUntil: cooldownUntil,
	}, nil
}

// BuildHandoffMarkdown formats the conversation cache for a dying session
// into a markdown prompt that re-establishes context for a fresh Devin
// session running under a different key.
//
// The format is intentionally human-readable so Devin can reason about it
// without any special handling, and so the user can read it directly in the
// UI when they expand a handoff row.
func BuildHandoffMarkdown(
	task tasks.Task,
	dying sessions.Session,
	msgs []sessions.Message,
	reason RotateReason,
	detail string,
	now time.Time,
) string {
	var b strings.Builder
	b.WriteString("# Handoff from previous session\n\n")
	b.WriteString("You are picking up a task that was started on a different API key. ")
	switch reason {
	case ReasonQuotaExhausted:
		b.WriteString("The previous key ran out of quota mid-task.")
	case ReasonManual:
		b.WriteString("The user manually rotated to a fresh key.")
	default:
		b.WriteString("The previous session was retired.")
	}
	b.WriteString("\n\n")

	fmt.Fprintf(&b, "- Task title: **%s**\n", task.Title)
	fmt.Fprintf(&b, "- Original task created: %s\n", task.CreatedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "- Previous session id: `%s`\n", dying.ID)
	if dying.DevinSessionID != "" {
		fmt.Fprintf(&b, "- Previous Devin session id: `%s`\n", dying.DevinSessionID)
	}
	fmt.Fprintf(&b, "- Rotation reason: %s — %s\n", reason, detail)
	fmt.Fprintf(&b, "- Rotation time (UTC): %s\n\n", now.Format(time.RFC3339))

	b.WriteString("## Original task prompt\n\n")
	b.WriteString("```\n")
	b.WriteString(task.InitialPrompt)
	b.WriteString("\n```\n\n")

	if len(msgs) == 0 {
		b.WriteString("## Conversation history\n\n_No messages had been exchanged on the previous session._\n\n")
	} else {
		sorted := append([]sessions.Message(nil), msgs...)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Timestamp.Before(sorted[j].Timestamp) })
		fmt.Fprintf(&b, "## Conversation history (%d message(s))\n\n", len(sorted))
		for _, m := range sorted {
			role := roleLabel(m.Role)
			fmt.Fprintf(&b, "### %s — %s\n\n", role, m.Timestamp.UTC().Format("2006-01-02 15:04:05 UTC"))
			b.WriteString(strings.TrimSpace(m.Content))
			b.WriteString("\n\n")
		}
	}

	b.WriteString("## What to do next\n\n")
	b.WriteString("Continue working on the original task using the conversation above as context. ")
	b.WriteString("Do not repeat work the previous session has already completed. ")
	b.WriteString("If you need clarification, address the user directly in your next message.\n")
	return b.String()
}

func roleLabel(r sessions.Role) string {
	switch r {
	case sessions.RoleUser:
		return "User"
	case sessions.RoleAssistant:
		return "Devin"
	case sessions.RoleSystem:
		return "System note"
	default:
		return string(r)
	}
}
