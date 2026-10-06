package console

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

// pollOverlap is how far before the newest item seen each poll starts, so a
// row that committed late with an earlier timestamp is still picked up.
// Items are de-duplicated by key.
const pollOverlap = 5 * time.Second

// streamEvent is one Server-Sent Event payload: the rendered timeline entry
// and what the communication map should highlight.
type streamEvent struct {
	HTML string `json:"html"`
	Kind string `json:"kind"`
	Seat string `json:"seat,omitempty"`
	Peer string `json:"peer,omitempty"`
}

// stream sends the organisation's new activity as Server-Sent Events until
// the client disconnects. ?after= is the newest item the page already shows.
func (s *server) stream(w http.ResponseWriter, r *http.Request) {
	ctx, orgID := r.Context(), r.PathValue("id")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	cursor := s.Now()
	if v := r.URL.Query().Get("after"); v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			cursor = t
		}
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, "retry: 3000\n\n")
	flusher.Flush()

	// The page already shows everything up to the initial cursor; the first
	// poll overlaps it, so those items are only marked as seen.
	initial, first := cursor, true
	seen := map[string]time.Time{}
	tick := time.NewTicker(s.PollInterval)
	defer tick.Stop()
	for {
		items, err := s.Platform.Activity(ctx, orgID, cursor.Add(-pollOverlap), 500)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.Log.WarnContext(ctx, "activity poll failed", "organization_id", orgID, "error", err)
		}
		for _, it := range items {
			if _, dup := seen[it.Key]; dup {
				continue
			}
			seen[it.Key] = it.At
			if first && !it.At.After(initial) {
				continue
			}
			if it.At.After(cursor) {
				cursor = it.At
			}
			if err := s.send(w, orgID, it); err != nil {
				return
			}
		}
		if err == nil {
			first = false
		}
		for k, at := range seen {
			if at.Before(cursor.Add(-2 * pollOverlap)) {
				delete(seen, k)
			}
		}
		// A comment keeps idle connections open through proxies.
		if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
			return
		}
		flusher.Flush()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (s *server) send(w http.ResponseWriter, orgID string, it runtimeapi.ConsoleActivityItem) error {
	html, err := s.fragment("activity-item", map[string]any{"I": it, "OrgID": orgID, "Now": s.Now()})
	if err != nil {
		return err
	}
	b, err := json.Marshal(streamEvent{HTML: html, Kind: it.Kind, Seat: it.SeatKey, Peer: it.PeerSeat})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: activity\ndata: %s\n\n", b)
	return err
}
