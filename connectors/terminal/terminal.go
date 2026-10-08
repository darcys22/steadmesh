// Package terminal is the bare-bones command-line communication adapter. A
// human connects to the platform (orgctl chat) instead of the platform
// dialling out: POST /messages sends a line to the representative and
// GET /events streams the representative's messages as Server-Sent Events.
//
// The connection secret maps each external user ID to that user's bearer
// token. Messages sent while the user has no stream open wait in a bounded
// in-memory queue; they are lost if the platform restarts or the connection
// is rebuilt.
package terminal

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/darcys22/steadmesh/connectors"
)

// ChannelID is the external channel of every terminal conversation; each
// user has one conversation.
const ChannelID = "terminal"

const (
	queueCap     = 200
	sentCap      = 1000
	maxBody      = 64 << 10
	heartbeat    = 30 * time.Second
	streamBuffer = 64
)

// Event is one message streamed to the human on GET /events.
type Event struct {
	ID   string    `json:"id"`
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// Inbound is the body of POST /messages. ID is chosen by the client and
// reused on retry so a resent line is delivered once.
type Inbound struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// Adapter implements connectors.Communication and connectors.HTTPIngress.
type Adapter struct {
	cfg    connectors.Config
	tokens map[string]string // user ID -> token
	log    *slog.Logger
	now    func() time.Time

	mu      sync.Mutex
	sink    connectors.IngressSink
	streams map[string]map[chan Event]struct{}
	queued  map[string][]Event
	sent    map[string]bool
	order   []string // sent keys, oldest first
}

var (
	_ connectors.Communication = (*Adapter)(nil)
	_ connectors.HTTPIngress   = (*Adapter)(nil)
)

// New builds a terminal adapter. cfg.Secret maps user IDs to tokens and must
// not be empty.
func New(cfg connectors.Config) (connectors.Communication, error) {
	return newAdapter(cfg)
}

func newAdapter(cfg connectors.Config) (*Adapter, error) {
	tokens := map[string]string{}
	for user, tok := range cfg.Secret {
		if user == "" || strings.TrimSpace(tok) == "" {
			continue
		}
		tokens[user] = strings.TrimSpace(tok)
	}
	if len(tokens) == 0 {
		return nil, fmt.Errorf("%w: terminal secret must map at least one user ID to a token", connectors.ErrUnauthorized)
	}
	return &Adapter{
		cfg:     cfg,
		tokens:  tokens,
		log:     slog.Default().With("connector", "terminal", "connection", cfg.Key),
		now:     time.Now,
		streams: map[string]map[chan Event]struct{}{},
		queued:  map[string][]Event{},
		sent:    map[string]bool{},
	}, nil
}

// Verify checks nothing external; the tokens were checked in New.
func (a *Adapter) Verify(context.Context) error { return nil }

// VerifyUser checks that userID has a token.
func (a *Adapter) VerifyUser(_ context.Context, userID string) error {
	if _, ok := a.tokens[userID]; !ok {
		return fmt.Errorf("%w: no terminal token for user %q", connectors.ErrPermanent, userID)
	}
	return nil
}

// Run accepts messages through Handler until ctx is cancelled.
func (a *Adapter) Run(ctx context.Context, sink connectors.IngressSink) error {
	a.mu.Lock()
	a.sink = sink
	a.mu.Unlock()
	<-ctx.Done()
	a.mu.Lock()
	a.sink = nil
	a.mu.Unlock()
	return ctx.Err()
}

// Healthy reports whether Run is accepting messages.
func (a *Adapter) Healthy() (bool, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sink == nil {
		return false, "not started"
	}
	return true, ""
}

// Send streams msg to the user's open terminals, or queues it until one
// connects. The receipt is the idempotency key.
func (a *Adapter) Send(_ context.Context, msg connectors.OutboundMessage) (string, error) {
	if _, ok := a.tokens[msg.ExternalUserID]; !ok {
		return "", fmt.Errorf("%w: no terminal token for user %q", connectors.ErrPermanent, msg.ExternalUserID)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if msg.IdempotencyKey != "" {
		if a.sent[msg.IdempotencyKey] {
			return msg.IdempotencyKey, nil
		}
		a.sent[msg.IdempotencyKey] = true
		a.order = append(a.order, msg.IdempotencyKey)
		if len(a.order) > sentCap {
			delete(a.sent, a.order[0])
			a.order = a.order[1:]
		}
	}
	ev := Event{ID: msg.IdempotencyKey, Text: msg.Text, At: a.now().UTC()}
	if subs := a.streams[msg.ExternalUserID]; len(subs) > 0 {
		for ch := range subs {
			select {
			case ch <- ev:
			default:
				a.log.Warn("terminal stream is not keeping up; message dropped for that stream", "user_id", msg.ExternalUserID)
			}
		}
		return msg.IdempotencyKey, nil
	}
	q := append(a.queued[msg.ExternalUserID], ev)
	if len(q) > queueCap {
		a.log.Warn("terminal queue full; oldest message dropped", "user_id", msg.ExternalUserID)
		q = q[1:]
	}
	a.queued[msg.ExternalUserID] = q
	return msg.IdempotencyKey, nil
}

// Handler serves POST /messages and GET /events, authenticated by the
// user's bearer token.
func (a *Adapter) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /messages", a.authed(a.post))
	mux.HandleFunc("GET /events", a.authed(a.events))
	return mux
}

func (a *Adapter) authed(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		user, ok := a.userFor(strings.TrimSpace(tok))
		if !ok {
			http.Error(w, "missing or invalid token", http.StatusUnauthorized)
			return
		}
		next(w, r, user)
	}
}

// userFor compares tok against every user's token in constant time.
func (a *Adapter) userFor(tok string) (string, bool) {
	if tok == "" {
		return "", false
	}
	var found string
	for user, want := range a.tokens {
		if subtle.ConstantTimeCompare([]byte(tok), []byte(want)) == 1 {
			found = user
		}
	}
	return found, found != ""
}

func (a *Adapter) post(w http.ResponseWriter, r *http.Request, user string) {
	var in Inbound
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&in); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if in.ID == "" || strings.TrimSpace(in.Text) == "" {
		http.Error(w, "id and text are required", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	sink := a.sink
	a.mu.Unlock()
	if sink == nil {
		http.Error(w, "terminal connection is not running", http.StatusServiceUnavailable)
		return
	}
	now := a.now()
	ev := connectors.InboundEvent{
		EventID: "terminal:" + user + ":" + in.ID, AccountID: a.cfg.AccountID, UserID: user,
		ChannelID: ChannelID, Text: in.Text, MessageTS: strconv.FormatInt(now.UnixNano(), 10),
	}
	if err := sink.Accept(r.Context(), a.cfg.Key, ev); err != nil {
		a.log.Error("terminal message not accepted", "user_id", user, "error", err)
		http.Error(w, "message not accepted; retry", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (a *Adapter) events(w http.ResponseWriter, r *http.Request, user string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch := make(chan Event, streamBuffer)
	a.mu.Lock()
	backlog := a.queued[user]
	delete(a.queued, user)
	if a.streams[user] == nil {
		a.streams[user] = map[chan Event]struct{}{}
	}
	a.streams[user][ch] = struct{}{}
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.streams[user], ch)
		if len(a.streams[user]) == 0 {
			delete(a.streams, user)
		}
		a.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	write := func(ev Event) error {
		b, _ := json.Marshal(ev)
		_, err := fmt.Fprintf(w, "data: %s\n\n", b)
		return err
	}
	for _, ev := range backlog {
		if err := write(ev); err != nil {
			a.requeue(user, backlog)
			return
		}
	}
	flusher.Flush()
	tick := time.NewTicker(heartbeat)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			if err := write(ev); err != nil {
				return
			}
			flusher.Flush()
		case <-tick.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// requeue puts undelivered backlog back in front of the user's queue.
func (a *Adapter) requeue(user string, evs []Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	q := append(append([]Event{}, evs...), a.queued[user]...)
	if len(q) > queueCap {
		q = q[len(q)-queueCap:]
	}
	a.queued[user] = q
}
