// Package events implements a tiny in-process broadcast bus used to push
// state changes (key state, session messages, artifact downloads, …) to
// subscribers in real time. The HTTP layer exposes the bus as an
// SSE endpoint at /events/stream so the UI can update without polling.
//
// The bus is not durable: subscribers only see events emitted after they
// subscribed. For backfill / late joiners we still rely on the database
// and HTMX polling endpoints. The bus is purely a "wake up, something
// changed" channel — payloads are intentionally small so we can fan out
// thousands per second without blocking publishers.
package events

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Kind tags the event so the client can route it. Keep these short and
// stable; the SSE wire format uses them as the `event:` field.
type Kind string

const (
	// KindKeyStateChanged fires when a key flips between active /
	// cooldown / dead / etc. Payload: {key_id, new_state, label}.
	KindKeyStateChanged Kind = "key.state_changed"
	// KindSessionMessage fires when a new chat message is appended to
	// any session (poller, user, system). Payload: {session_id, role,
	// content_preview}.
	KindSessionMessage Kind = "session.message_appended"
	// KindArtifactReady fires when an artifact finishes downloading.
	// Payload: {artifact_id, session_id, filename, content_type}.
	KindArtifactReady Kind = "artifact.ready"
	// KindHandoffLinked fires when a rotation finishes and a new
	// session is linked to a handoff row. Payload: {handoff_id,
	// from_session, to_session, task_id}.
	KindHandoffLinked Kind = "handoff.linked"
	// KindTaskStatus fires when a task flips status (running / paused
	// / completed / failed). Payload: {task_id, status}.
	KindTaskStatus Kind = "task.status_changed"
	// KindNotification fires for user-visible notification entries
	// (PR-8 notifications.Repo writes). Payload: {event_id, kind,
	// title, body}. Surfaced as browser Notification when the SSE
	// stream is open.
	KindNotification Kind = "notification"
)

// Event is one broadcast item. Data is a small map so we don't depend on
// concrete struct types from downstream packages — the bus has zero
// imports from internal/{keys,sessions,...}.
type Event struct {
	ID   uint64         `json:"id"`
	Kind Kind           `json:"kind"`
	At   time.Time      `json:"at"`
	Data map[string]any `json:"data,omitempty"`
}

// Bus is a fan-out broker. Publishers call Publish; subscribers receive
// from the channel returned by Subscribe. Subscribers MUST drain their
// channel or be unsubscribed promptly — slow consumers will see drops
// (we never block the publisher because that would stall the manager).
type Bus struct {
	mu      sync.RWMutex
	subs    map[uint64]chan Event
	next    uint64 // monotonic subscriber ID
	eventID atomic.Uint64
}

// NewBus returns an empty bus. The zero value is not usable — always
// call this constructor so the subs map is initialised.
func NewBus() *Bus {
	return &Bus{subs: make(map[uint64]chan Event)}
}

// Publish broadcasts an event to all current subscribers. Never blocks:
// if a subscriber's buffer is full we drop the event for that subscriber
// (the SSE handler logs the drop so we don't silently lose updates on a
// frozen client).
func (b *Bus) Publish(kind Kind, data map[string]any) {
	if b == nil {
		return
	}
	ev := Event{
		ID:   b.eventID.Add(1),
		Kind: kind,
		At:   time.Now().UTC(),
		Data: data,
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default:
			// Subscriber is wedged — skip. The SSE goroutine that
			// owns this channel will notice the gap (event IDs are
			// monotonic) and may decide to issue an HTMX refresh.
		}
	}
}

// Subscribe registers a new subscriber and returns a cancel function +
// receive channel. The channel is buffered so a brief burst of events
// (e.g. one per message in a 10-message reply) doesn't drop on a
// healthy consumer. ctx cancellation triggers automatic unsubscribe.
func (b *Bus) Subscribe(ctx context.Context) (<-chan Event, func()) {
	if b == nil {
		ch := make(chan Event)
		close(ch)
		return ch, func() {}
	}
	b.mu.Lock()
	id := b.next
	b.next++
	ch := make(chan Event, 32)
	b.subs[id] = ch
	b.mu.Unlock()

	cancel := func() {
		b.mu.Lock()
		if existing, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(existing)
		}
		b.mu.Unlock()
	}
	go func() {
		<-ctx.Done()
		cancel()
	}()
	return ch, cancel
}

// SubscriberCount returns the number of live subscribers. Exposed for
// diagnostics + tests.
func (b *Bus) SubscriberCount() int {
	if b == nil {
		return 0
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}
