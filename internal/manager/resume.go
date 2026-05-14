package manager

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ggwpgoend/devin-key-manager/internal/devin"
	"github.com/ggwpgoend/devin-key-manager/internal/sessions"
)

// PR-18 / roadmap B20: smart resume.
//
// When a Devin session "drops" — either because we observed it stuck
// in `running` for a long time, or it terminated with a recoverable
// error — we want to (a) figure out *why* and (b) automatically push
// it forward without the user having to type "continue" every time.
//
// The diagnosis is deliberately rule-based and observable. We never
// invent new prompts; the worst case is a "please continue from where
// you left off" follow-up.

// ResumeReason classifies why we think a session needs a nudge.
type ResumeReason string

const (
	ResumeReasonIdle           ResumeReason = "idle"           // no new messages for too long
	ResumeReasonQuotaExhausted ResumeReason = "quota"          // key exhausted; needs handoff
	ResumeReasonFailed         ResumeReason = "failed"         // session marked failed but task isn't terminal
	ResumeReasonStuckCreating  ResumeReason = "stuck_creating" // never advanced past `creating`
	ResumeReasonHealthy        ResumeReason = "healthy"        // no action needed
	ResumeReasonTerminal       ResumeReason = "terminal"       // session is done; nothing to resume
)

// ResumeAction describes what should be done next.
type ResumeAction string

const (
	ActionNone       ResumeAction = "none"        // healthy or terminal
	ActionContinue   ResumeAction = "continue"    // send a "continue" follow-up
	ActionHandoff    ResumeAction = "handoff"     // rotate to a fresh key
	ActionFail       ResumeAction = "fail"        // give up, mark task failed
)

// Diagnosis is the structured outcome of analysing a session.
type Diagnosis struct {
	SessionID string
	Reason    ResumeReason
	Action    ResumeAction
	Detail    string
	// MessagesSeen is how many messages we have locally for this session.
	MessagesSeen int
	// IdleFor is the wall-clock gap between now and the last message.
	IdleFor time.Duration
}

// DiagnoseSession analyses a single session and returns what should
// happen next. Pure — no side effects. Callers can show this to the
// user, or pass to ResumeSession to act on it.
func (m *Manager) DiagnoseSession(ctx context.Context, sessionID string) (Diagnosis, error) {
	sess, err := m.sessions.Get(ctx, sessionID)
	if err != nil {
		return Diagnosis{}, err
	}
	d := Diagnosis{SessionID: sessionID}
	// Terminal states first — nothing to resume.
	switch sess.Status {
	case sessions.StatusCompleted, sessions.StatusHandoffDone:
		d.Reason = ResumeReasonTerminal
		d.Action = ActionNone
		d.Detail = string(sess.Status)
		return d, nil
	case sessions.StatusFailed:
		d.Reason = ResumeReasonFailed
		d.Action = ActionHandoff
		d.Detail = "session failed: " + sess.EndReason
		return d, nil
	case sessions.StatusQuotaExhausted:
		d.Reason = ResumeReasonQuotaExhausted
		d.Action = ActionHandoff
		d.Detail = "key quota exhausted"
		return d, nil
	}

	msgs, err := m.sessions.ListMessages(ctx, sessionID)
	if err != nil {
		return d, err
	}
	d.MessagesSeen = len(msgs)
	now := m.now().UTC()

	// Stuck in `creating` for more than 5 minutes — almost certainly
	// a hung create; recommend handoff so the task can run on a
	// healthy key.
	if sess.Status == sessions.StatusCreating {
		age := now.Sub(sess.StartedAt.UTC())
		if age > 5*time.Minute {
			d.Reason = ResumeReasonStuckCreating
			d.Action = ActionHandoff
			d.Detail = fmt.Sprintf("stuck in creating for %s", age.Round(time.Second))
			return d, nil
		}
		d.Reason = ResumeReasonHealthy
		d.Action = ActionNone
		d.Detail = "creating (within grace window)"
		return d, nil
	}

	// Idle detection: if the last message is older than the
	// configurable idle threshold AND the session is still open
	// (running/blocked), recommend a "continue" nudge.
	if len(msgs) == 0 {
		// No messages but session is open — uncommon; recommend
		// a sync first via a continue ping.
		d.Reason = ResumeReasonIdle
		d.Action = ActionContinue
		d.Detail = "no messages cached yet"
		return d, nil
	}
	last := msgs[len(msgs)-1]
	d.IdleFor = now.Sub(last.Timestamp.UTC())
	if d.IdleFor > 10*time.Minute {
		d.Reason = ResumeReasonIdle
		d.Action = ActionContinue
		d.Detail = fmt.Sprintf("no new messages for %s", d.IdleFor.Round(time.Second))
		return d, nil
	}
	d.Reason = ResumeReasonHealthy
	d.Action = ActionNone
	d.Detail = "no intervention needed"
	return d, nil
}

// ResumeSession runs DiagnoseSession and then executes the recommended
// action. Returns the diagnosis it acted on plus any error from the
// action itself.
func (m *Manager) ResumeSession(ctx context.Context, sessionID string) (Diagnosis, error) {
	d, err := m.DiagnoseSession(ctx, sessionID)
	if err != nil {
		return d, err
	}
	switch d.Action {
	case ActionNone:
		return d, nil
	case ActionContinue:
		msg := "Please continue from where you left off."
		if d.Detail != "" {
			msg = "Devin manager: " + d.Detail + ". Please continue from where you left off."
		}
		if err := m.SendFollowUp(ctx, sessionID, msg); err != nil {
			// If sending hits quota, we already kicked off a handoff
			// inside SendFollowUp; surface the error but report the
			// diagnosis honestly.
			if errors.Is(err, devin.ErrQuotaExhausted) {
				d.Action = ActionHandoff
				d.Detail = "send_message quota_exhausted; rotated"
				return d, nil
			}
			return d, fmt.Errorf("manager: resume send: %w", err)
		}
		return d, nil
	case ActionHandoff:
		sess, err := m.sessions.Get(ctx, sessionID)
		if err != nil {
			return d, err
		}
		if _, err := m.rotateOnQuota(ctx, sess, "resume: "+strings.TrimSpace(d.Detail)); err != nil {
			return d, fmt.Errorf("manager: resume rotate: %w", err)
		}
		return d, nil
	case ActionFail:
		if err := m.sessions.SetStatus(ctx, sessionID, sessions.StatusFailed, d.Detail); err != nil {
			return d, err
		}
		return d, nil
	}
	return d, nil
}
