package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ggwpgoend/devin-key-manager/internal/events"
)

// handleEventsStream serves the SSE long-lived stream. Each subscriber gets a
// dedicated bus subscription that auto-cancels when the HTTP client
// disconnects (via request context). We also send a heartbeat comment every
// ~25s so proxies don't time out and clients can detect dead connections.
//
// Wire format:
//
//	event: <kind>
//	id: <monotonic-id>
//	data: <json>\n\n
//
// Heartbeats are written as `: ping` comments (per the SSE spec — comment
// lines start with a colon and are ignored by EventSource).
func (s *Server) handleEventsStream(w http.ResponseWriter, r *http.Request) {
	if s.bus == nil {
		http.Error(w, "live events disabled (no bus configured)", http.StatusNotFound)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		// Should only happen behind a buffered intermediary; SSE needs
		// flush to be useful.
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // disable nginx proxy buffering
	w.WriteHeader(http.StatusOK)

	// Send an initial retry hint so the browser auto-reconnects after a
	// network blip (default is 3s anyway, but being explicit is safer).
	if _, err := io.WriteString(w, "retry: 3000\n\n"); err != nil {
		return
	}
	flusher.Flush()

	ch, cancel := s.bus.Subscribe(r.Context())
	defer cancel()

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if err := writeSSEEvent(w, ev); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeSSEEvent serialises one bus event into SSE wire format.
func writeSSEEvent(w io.Writer, ev events.Event) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	// `event:` + `id:` + `data:` triplet. The blank line is the record
	// terminator; without it browsers buffer indefinitely.
	if _, err := fmt.Fprintf(w, "event: %s\nid: %d\ndata: %s\n\n", ev.Kind, ev.ID, body); err != nil {
		return err
	}
	return nil
}
