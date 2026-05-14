package events

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBus_PublishSubscribe(t *testing.T) {
	t.Parallel()
	b := NewBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, _ := b.Subscribe(ctx)
	b.Publish(KindKeyStateChanged, map[string]any{"key_id": "k1", "new_state": "active"})

	select {
	case ev := <-ch:
		if ev.Kind != KindKeyStateChanged {
			t.Fatalf("kind=%q want %q", ev.Kind, KindKeyStateChanged)
		}
		if ev.Data["key_id"] != "k1" {
			t.Fatalf("data missing key_id: %#v", ev.Data)
		}
		if ev.ID == 0 {
			t.Fatalf("event ID should be monotonic, got 0")
		}
	case <-time.After(time.Second):
		t.Fatal("publish never delivered to subscriber")
	}
}

func TestBus_MultipleSubscribersAllReceive(t *testing.T) {
	t.Parallel()
	b := NewBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch1, _ := b.Subscribe(ctx)
	ch2, _ := b.Subscribe(ctx)
	if c := b.SubscriberCount(); c != 2 {
		t.Fatalf("subscribers=%d want 2", c)
	}

	b.Publish(KindSessionMessage, map[string]any{"session_id": "s1"})

	for i, ch := range []<-chan Event{ch1, ch2} {
		select {
		case ev := <-ch:
			if ev.Kind != KindSessionMessage {
				t.Fatalf("sub %d kind=%q", i, ev.Kind)
			}
		case <-time.After(time.Second):
			t.Fatalf("sub %d did not receive", i)
		}
	}
}

func TestBus_UnsubscribeOnContextCancel(t *testing.T) {
	t.Parallel()
	b := NewBus()
	ctx, cancel := context.WithCancel(context.Background())
	ch, _ := b.Subscribe(ctx)
	if c := b.SubscriberCount(); c != 1 {
		t.Fatalf("pre-cancel count=%d want 1", c)
	}
	cancel()
	// Give the auto-unsubscribe goroutine a chance to run.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if b.SubscriberCount() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if c := b.SubscriberCount(); c != 0 {
		t.Fatalf("post-cancel count=%d want 0", c)
	}
	// Channel should be closed after unsubscribe.
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected channel closed after cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("channel never closed after cancel")
	}
}

func TestBus_PublishDoesNotBlockOnSlowSubscriber(t *testing.T) {
	t.Parallel()
	b := NewBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _ = b.Subscribe(ctx) // never reads — buffer fills then drops

	// Push way more events than the per-sub buffer (32). The test
	// passes if we don't hang.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			b.Publish(KindKeyStateChanged, map[string]any{"i": i})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publish wedged on slow subscriber")
	}
}

func TestBus_PublishParallel(t *testing.T) {
	t.Parallel()
	b := NewBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, _ := b.Subscribe(ctx)

	var received atomic.Int64
	go func() {
		for range ch {
			received.Add(1)
		}
	}()
	var wg sync.WaitGroup
	const goroutines = 8
	const each = 50
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				b.Publish(KindKeyStateChanged, map[string]any{"i": i})
			}
		}()
	}
	wg.Wait()
	time.Sleep(100 * time.Millisecond) // let reader drain
	// Drops are expected when readers fall behind the per-sub buffer (32);
	// this test only asserts that publishes do not deadlock and at least
	// one event is delivered. The bus is best-effort, not durable.
	if r := received.Load(); r == 0 {
		t.Fatalf("no events received from parallel publishers")
	}
}

func TestNilBus_SafeNoOps(t *testing.T) {
	t.Parallel()
	var b *Bus
	b.Publish(KindKeyStateChanged, nil) // must not panic
	ch, cancel := b.Subscribe(context.Background())
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("nil bus subscribe should return closed channel")
		}
	default:
		// closed channel returns immediately; allow ok=false above
		t.Fatal("nil bus subscribe channel not closed")
	}
	if b.SubscriberCount() != 0 {
		t.Fatal("nil bus count should be 0")
	}
}
