// Package fakeslack is a deterministic fake of the Slack Web API subset and the
// Socket Mode websocket used by connectors/slack. It is served from tests via
// httptest and from cmd/fakes inside kind.
//
// Web API (under /api/): auth.test, users.info, conversations.open,
// chat.postMessage, apps.connections.open. Socket Mode: /link/?ticket=...
//
// Test control:
//
//	POST /_test/dm          {user, text, event_id?, thread_ts?, channel?, bot_id?, subtype?, team_id?}
//	GET  /_test/posted      posted messages
//	GET  /_test/acks        recorded socket-mode acks
//	GET  /_test/envelopes   delivery state of every envelope
//	GET  /_test/connections number of open socket-mode connections
//	POST /_test/fail        {method, mode: "ambiguous"|"rate_limited"|"server_error"|<slack error>, count}
//	POST /_test/disconnect  close all socket-mode connections
//	POST /_test/reset       clear posted messages, acks, envelopes and failures
package fakeslack

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Options configure the fake. Zero values take the documented defaults.
type Options struct {
	TeamID    string // default T0FAKE
	TeamName  string // default fake
	BotUserID string // default U0BOT
	BotID     string // default B0BOT
	AppID     string // default A0FAKE
	// BotToken and AppToken, when set, must match exactly. Otherwise any
	// xoxb- / xapp- token is accepted.
	BotToken string
	AppToken string
	// Users maps user id to name. Default: U0ALICE alice, U0BOB bob.
	Users map[string]string
	// RedeliveryTimeout is how long an envelope may stay unacked before it is
	// redelivered. Default 3s.
	RedeliveryTimeout time.Duration
	// MaxRetries is the number of redeliveries after the first attempt.
	// Default 3; a negative value disables redelivery.
	MaxRetries int
	// PingInterval is how often the server pings each websocket. Default 5s.
	PingInterval time.Duration
}

func (o *Options) defaults() {
	if o.TeamID == "" {
		o.TeamID = "T0FAKE"
	}
	if o.TeamName == "" {
		o.TeamName = "fake"
	}
	if o.BotUserID == "" {
		o.BotUserID = "U0BOT"
	}
	if o.BotID == "" {
		o.BotID = "B0BOT"
	}
	if o.AppID == "" {
		o.AppID = "A0FAKE"
	}
	if o.Users == nil {
		o.Users = map[string]string{"U0ALICE": "alice", "U0BOB": "bob"}
	}
	if o.RedeliveryTimeout <= 0 {
		o.RedeliveryTimeout = 3 * time.Second
	}
	if o.MaxRetries < 0 {
		o.MaxRetries = 0
	} else if o.MaxRetries == 0 {
		o.MaxRetries = 3
	}
	if o.PingInterval <= 0 {
		o.PingInterval = 5 * time.Second
	}
}

// DM is a direct message injected by a test.
type DM struct {
	User     string `json:"user"`
	Text     string `json:"text"`
	EventID  string `json:"event_id,omitempty"`
	ThreadTS string `json:"thread_ts,omitempty"`
	Channel  string `json:"channel,omitempty"`
	BotID    string `json:"bot_id,omitempty"`
	Subtype  string `json:"subtype,omitempty"`
	TeamID   string `json:"team_id,omitempty"`
}

// Injected describes the envelope created for a DM.
type Injected struct {
	EventID    string `json:"event_id"`
	EnvelopeID string `json:"envelope_id"`
	TS         string `json:"ts"`
	Channel    string `json:"channel"`
}

// PostedMessage is a chat.postMessage call the fake accepted.
type PostedMessage struct {
	Channel  string          `json:"channel"`
	Text     string          `json:"text"`
	ThreadTS string          `json:"thread_ts,omitempty"`
	TS       string          `json:"ts"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

// Ack is a socket-mode acknowledgement received from a client.
type Ack struct {
	EnvelopeID   string    `json:"envelope_id"`
	EventID      string    `json:"event_id"`
	RetryAttempt int       `json:"retry_attempt"`
	At           time.Time `json:"at"`
}

// EnvelopeState is the delivery state of one envelope.
type EnvelopeState struct {
	EnvelopeID string `json:"envelope_id"`
	EventID    string `json:"event_id"`
	Attempts   int    `json:"attempts"` // deliveries so far
	Acked      bool   `json:"acked"`
	GaveUp     bool   `json:"gave_up"`
}

type envelope struct {
	EnvelopeState
	payload json.RawMessage
	sentAt  time.Time
}

type failRule struct {
	mode      string
	remaining int
}

type wsConn struct {
	c  *websocket.Conn
	mu sync.Mutex
}

func (w *wsConn) writeJSON(v any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return w.c.WriteJSON(v)
}

func (w *wsConn) ping() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.c.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(5*time.Second))
}

// Server is the fake. It implements http.Handler.
type Server struct {
	opts Options
	// epoch makes generated event ids unique across restarts, as Slack's are;
	// otherwise a restarted fake would reuse ids the platform has already seen.
	epoch    string
	mux      *http.ServeMux
	upgrader websocket.Upgrader

	mu        sync.Mutex
	seq       int
	envelopes []*envelope
	posted    []PostedMessage
	acks      []Ack
	fails     map[string][]*failRule
	conns     []*wsConn

	kick chan struct{}
	done chan struct{}
	once sync.Once
}

// New creates the fake and starts its redelivery loop. Call Close to stop it.
func New(opts Options) *Server {
	opts.defaults()
	s := &Server{
		opts:  opts,
		epoch: strconv.FormatInt(time.Now().UnixNano(), 36),
		mux:   http.NewServeMux(),
		fails: map[string][]*failRule{},
		kick:  make(chan struct{}, 1),
		done:  make(chan struct{}),
	}
	// slack-go sends Origin: https://api.slack.com.
	s.upgrader.CheckOrigin = func(*http.Request) bool { return true }
	s.mux.HandleFunc("/api/", s.handleAPI)
	s.mux.HandleFunc("/link/", s.handleLink)
	s.mux.HandleFunc("/link", s.handleLink)
	s.mux.HandleFunc("POST /_test/dm", s.handleTestDM)
	s.mux.HandleFunc("GET /_test/posted", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, s.Posted()) })
	s.mux.HandleFunc("GET /_test/acks", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, s.Acks()) })
	s.mux.HandleFunc("GET /_test/envelopes", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, s.Envelopes()) })
	s.mux.HandleFunc("GET /_test/connections", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]int{"connected": s.Connections()})
	})
	s.mux.HandleFunc("POST /_test/fail", s.handleTestFail)
	s.mux.HandleFunc("POST /_test/disconnect", func(w http.ResponseWriter, _ *http.Request) {
		s.Disconnect()
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	s.mux.HandleFunc("POST /_test/reset", func(w http.ResponseWriter, _ *http.Request) {
		s.Reset()
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	go s.deliveryLoop()
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// Close stops background work and closes websockets.
func (s *Server) Close() {
	s.once.Do(func() { close(s.done) })
	s.Disconnect()
}

// TeamID returns the configured team id.
func (s *Server) TeamID() string { return s.opts.TeamID }

// BotUserID returns the bot's user id.
func (s *Server) BotUserID() string { return s.opts.BotUserID }

// Posted returns a copy of the posted messages.
func (s *Server) Posted() []PostedMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]PostedMessage{}, s.posted...)
}

// Acks returns a copy of the recorded acks.
func (s *Server) Acks() []Ack {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Ack{}, s.acks...)
}

// Envelopes returns the state of every envelope.
func (s *Server) Envelopes() []EnvelopeState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]EnvelopeState, 0, len(s.envelopes))
	for _, e := range s.envelopes {
		out = append(out, e.EnvelopeState)
	}
	return out
}

// Connections returns the number of open socket-mode connections.
func (s *Server) Connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// Fail makes the next count calls to a Web API method fail with mode.
func (s *Server) Fail(method, mode string, count int) {
	if count <= 0 {
		count = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fails[method] = append(s.fails[method], &failRule{mode: mode, remaining: count})
}

// Disconnect closes every socket-mode connection.
func (s *Server) Disconnect() {
	s.mu.Lock()
	conns := s.conns
	s.conns = nil
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.c.Close()
	}
}

// Reset clears recorded state and failure rules (connections stay open).
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.envelopes = nil
	s.posted = nil
	s.acks = nil
	s.fails = map[string][]*failRule{}
}

func (s *Server) nextLocked() int {
	s.seq++
	return s.seq
}

func (s *Server) tsLocked() string {
	return fmt.Sprintf("1700000000.%06d", s.nextLocked())
}

// DMChannel returns the deterministic DM channel id for a user.
func DMChannel(user string) string { return "D" + strings.TrimPrefix(user, "U") }

// InjectDM queues a message.im event. Reusing an EventID simulates a Slack
// replay: a new envelope with the same event id.
func (s *Server) InjectDM(dm DM) (Injected, error) {
	if dm.User == "" {
		return Injected{}, fmt.Errorf("user is required")
	}
	s.mu.Lock()
	n := s.nextLocked()
	if dm.EventID == "" {
		dm.EventID = fmt.Sprintf("Ev%s%06d", s.epoch, n)
	}
	channel := dm.Channel
	if channel == "" {
		channel = DMChannel(dm.User)
	}
	team := dm.TeamID
	if team == "" {
		team = s.opts.TeamID
	}
	ts := s.tsLocked()
	ev := map[string]any{
		"type":          "message",
		"channel":       channel,
		"user":          dm.User,
		"text":          dm.Text,
		"ts":            ts,
		"event_ts":      ts,
		"channel_type":  "im",
		"client_msg_id": fmt.Sprintf("cm-%08d", n),
	}
	if dm.ThreadTS != "" {
		ev["thread_ts"] = dm.ThreadTS
	}
	if dm.BotID != "" {
		ev["bot_id"] = dm.BotID
	}
	if dm.Subtype != "" {
		ev["subtype"] = dm.Subtype
	}
	payload, _ := json.Marshal(map[string]any{
		"token":      "fake-verification-token",
		"team_id":    team,
		"api_app_id": s.opts.AppID,
		"event":      ev,
		"type":       "event_callback",
		"event_id":   dm.EventID,
		"event_time": 1700000000 + n,
		"authorizations": []map[string]any{
			{"team_id": team, "user_id": s.opts.BotUserID, "is_bot": true},
		},
	})
	env := &envelope{
		EnvelopeState: EnvelopeState{
			EnvelopeID: fmt.Sprintf("env-%08d", n),
			EventID:    dm.EventID,
		},
		payload: payload,
	}
	s.envelopes = append(s.envelopes, env)
	s.mu.Unlock()
	s.poke()
	return Injected{EventID: dm.EventID, EnvelopeID: env.EnvelopeID, TS: ts, Channel: channel}, nil
}

func (s *Server) poke() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *Server) deliveryLoop() {
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-tick.C:
		case <-s.kick:
		}
		s.deliverDue()
	}
}

type outbound struct {
	conn *wsConn
	msg  map[string]any
}

func (s *Server) deliverDue() {
	now := time.Now()
	var out []outbound
	s.mu.Lock()
	if len(s.conns) > 0 {
		conn := s.conns[len(s.conns)-1]
		for _, e := range s.envelopes {
			if e.Acked || e.GaveUp {
				continue
			}
			if e.Attempts > 0 && now.Sub(e.sentAt) < s.opts.RedeliveryTimeout {
				continue
			}
			if e.Attempts > s.opts.MaxRetries {
				e.GaveUp = true
				continue
			}
			reason := ""
			if e.Attempts > 0 {
				reason = "timeout"
			}
			out = append(out, outbound{conn: conn, msg: map[string]any{
				"envelope_id":              e.EnvelopeID,
				"type":                     "events_api",
				"payload":                  e.payload,
				"accepts_response_payload": false,
				"retry_attempt":            e.Attempts,
				"retry_reason":             reason,
			}})
			e.Attempts++
			e.sentAt = now
		}
	}
	s.mu.Unlock()
	for _, o := range out {
		if err := o.conn.writeJSON(o.msg); err != nil {
			s.dropConn(o.conn)
		}
	}
}

func (s *Server) dropConn(c *wsConn) {
	s.mu.Lock()
	for i, x := range s.conns {
		if x == c {
			s.conns = append(s.conns[:i], s.conns[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
	_ = c.c.Close()
}

func (s *Server) handleLink(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("ticket") == "" {
		http.Error(w, "missing ticket", http.StatusUnauthorized)
		return
	}
	c, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn := &wsConn{c: c}
	if err := conn.writeJSON(map[string]any{
		"type":            "hello",
		"num_connections": 1,
		"connection_info": map[string]string{"app_id": s.opts.AppID},
		"debug_info":      map[string]any{"host": "fake", "approximate_connection_time": 3600},
	}); err != nil {
		_ = c.Close()
		return
	}
	_ = conn.ping()
	s.mu.Lock()
	s.conns = append(s.conns, conn)
	s.mu.Unlock()
	s.poke()

	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(s.opts.PingInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-s.done:
				return
			case <-t.C:
				if err := conn.ping(); err != nil {
					return
				}
			}
		}
	}()
	defer close(stop)
	defer s.dropConn(conn)
	for {
		var msg struct {
			EnvelopeID string `json:"envelope_id"`
		}
		if err := c.ReadJSON(&msg); err != nil {
			return
		}
		if msg.EnvelopeID == "" {
			continue
		}
		s.mu.Lock()
		ack := Ack{EnvelopeID: msg.EnvelopeID, At: time.Now()}
		for _, e := range s.envelopes {
			if e.EnvelopeID == msg.EnvelopeID {
				e.Acked = true
				ack.EventID = e.EventID
				ack.RetryAttempt = e.Attempts - 1
			}
		}
		s.acks = append(s.acks, ack)
		s.mu.Unlock()
	}
}

// ---- Web API -----------------------------------------------------------------

func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	method := strings.TrimPrefix(r.URL.Path, "/api/")
	_ = r.ParseForm()
	token := r.Form.Get("token")
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		token = strings.TrimPrefix(h, "Bearer ")
	}

	isApp := method == "apps.connections.open"
	if !s.tokenOK(token, isApp) {
		slackErr(w, "invalid_auth")
		return
	}

	mode := s.takeFail(method)
	switch mode {
	case "":
	case "rate_limited":
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"ok":false,"error":"ratelimited"}`))
		return
	case "server_error":
		w.WriteHeader(http.StatusInternalServerError)
		return
	case "ambiguous":
		// handled after the effect is applied
	default:
		slackErr(w, mode)
		return
	}

	var resp any
	switch method {
	case "auth.test":
		resp = map[string]any{
			"ok": true, "url": "https://" + s.opts.TeamName + ".slack.com/", "team": s.opts.TeamName,
			"user": "steadmesh", "team_id": s.opts.TeamID, "user_id": s.opts.BotUserID, "bot_id": s.opts.BotID,
		}
	case "users.info":
		id := r.Form.Get("user")
		name, ok := s.opts.Users[id]
		if !ok {
			slackErr(w, "user_not_found")
			return
		}
		resp = map[string]any{"ok": true, "user": map[string]any{
			"id": id, "team_id": s.opts.TeamID, "name": name, "real_name": name,
			"deleted": false, "is_bot": false,
		}}
	case "conversations.open":
		users := r.Form.Get("users")
		if users == "" || strings.Contains(users, ",") {
			slackErr(w, "users_list_not_supplied")
			return
		}
		if _, ok := s.opts.Users[users]; !ok {
			slackErr(w, "user_not_found")
			return
		}
		resp = map[string]any{"ok": true, "channel": map[string]any{"id": DMChannel(users), "is_im": true, "user": users}}
	case "chat.postMessage":
		ch := r.Form.Get("channel")
		if ch == "" {
			slackErr(w, "channel_not_found")
			return
		}
		if strings.HasPrefix(ch, "U") {
			ch = DMChannel(ch)
		}
		text := r.Form.Get("text")
		if text == "" {
			slackErr(w, "no_text")
			return
		}
		s.mu.Lock()
		pm := PostedMessage{Channel: ch, Text: text, ThreadTS: r.Form.Get("thread_ts"), TS: s.tsLocked()}
		if md := r.Form.Get("metadata"); md != "" && json.Valid([]byte(md)) {
			pm.Metadata = json.RawMessage(md)
		}
		s.posted = append(s.posted, pm)
		s.mu.Unlock()
		resp = map[string]any{"ok": true, "channel": ch, "ts": pm.TS, "message": map[string]any{
			"type": "message", "text": text, "user": s.opts.BotUserID, "bot_id": s.opts.BotID, "ts": pm.TS,
		}}
	case "apps.connections.open":
		scheme := "ws"
		if r.TLS != nil {
			scheme = "wss"
		}
		s.mu.Lock()
		n := s.nextLocked()
		s.mu.Unlock()
		resp = map[string]any{"ok": true, "url": fmt.Sprintf("%s://%s/link/?ticket=t%d", scheme, r.Host, n)}
	default:
		slackErr(w, "unknown_method")
		return
	}

	if mode == "ambiguous" {
		hijackClose(w)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) tokenOK(token string, app bool) bool {
	if app {
		if s.opts.AppToken != "" {
			return token == s.opts.AppToken
		}
		return strings.HasPrefix(token, "xapp-")
	}
	if s.opts.BotToken != "" {
		return token == s.opts.BotToken
	}
	return strings.HasPrefix(token, "xoxb-")
}

func (s *Server) takeFail(method string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	rules := s.fails[method]
	for len(rules) > 0 && rules[0].remaining <= 0 {
		rules = rules[1:]
	}
	s.fails[method] = rules
	if len(rules) == 0 {
		return ""
	}
	rules[0].remaining--
	return rules[0].mode
}

// ---- Test control -------------------------------------------------------------

func (s *Server) handleTestDM(w http.ResponseWriter, r *http.Request) {
	var dm DM
	if err := json.NewDecoder(r.Body).Decode(&dm); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	inj, err := s.InjectDM(dm)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, inj)
}

func (s *Server) handleTestFail(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string `json:"method"`
		Mode   string `json:"mode"`
		Count  int    `json:"count"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Method == "" || req.Mode == "" {
		http.Error(w, "method and mode are required", http.StatusBadRequest)
		return
	}
	s.Fail(req.Method, req.Mode, req.Count)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- helpers --------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func slackErr(w http.ResponseWriter, code string) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": code})
}

// hijackClose drops the connection without a response, simulating a response
// lost after the server committed the effect.
func hijackClose(w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		panic(http.ErrAbortHandler)
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	_ = conn.Close()
}
