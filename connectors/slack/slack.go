// Package slack is the Slack communication adapter (ADR-0003). Ingress uses
// Socket Mode, so the platform needs no public endpoint. An event is
// acknowledged only after the ingress sink has durably accepted it (§9.2).
package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/connectors/internal/httpx"
)

// MetadataEventType is the Slack message metadata event type that carries the
// platform operation id of an outbound message.
const MetadataEventType = "steadmesh_operation"

const (
	minBackoff = 500 * time.Millisecond
	maxBackoff = 60 * time.Second
)

// Adapter implements connectors.Communication for Slack.
type Adapter struct {
	cfg          connectors.Config
	api          *slack.Client
	pingInterval time.Duration
	log          *slog.Logger

	mu      sync.Mutex
	healthy bool
	detail  string
}

var _ connectors.Communication = (*Adapter)(nil)

// New builds a Slack adapter from cfg. cfg.Secret must hold bot_token (xoxb-)
// and app_token (xapp-). cfg.Endpoint, when set, replaces the Slack API base
// URL (e.g. http://fakes:8090 or http://fakes:8090/api/).
// cfg.Extra["ping_interval"] optionally overrides the websocket ping deadline.
func New(cfg connectors.Config) (connectors.Communication, error) {
	return newAdapter(cfg)
}

func newAdapter(cfg connectors.Config) (*Adapter, error) {
	bot, app := cfg.Secret["bot_token"], cfg.Secret["app_token"]
	if !strings.HasPrefix(bot, "xoxb-") {
		return nil, fmt.Errorf("%w: slack secret must contain bot_token (xoxb-)", connectors.ErrUnauthorized)
	}
	if !strings.HasPrefix(app, "xapp-") {
		return nil, fmt.Errorf("%w: slack secret must contain app_token (xapp-)", connectors.ErrUnauthorized)
	}
	opts := []slack.Option{
		slack.OptionAppLevelToken(app),
		slack.OptionHTTPClient(httpx.Client(cfg.HTTP)),
	}
	if cfg.Endpoint != "" {
		opts = append(opts, slack.OptionAPIURL(apiURL(cfg.Endpoint)))
	}
	a := &Adapter{
		cfg:          cfg,
		api:          slack.New(bot, opts...),
		pingInterval: 30 * time.Second,
		log:          slog.Default().With("connector", "slack", "connection", cfg.Key),
		detail:       "not started",
	}
	if v := cfg.Extra["ping_interval"]; v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("%w: invalid ping_interval %q", connectors.ErrPermanent, v)
		}
		a.pingInterval = d
	}
	return a, nil
}

// apiURL normalises an endpoint override to slack-go's form: ".../api/".
func apiURL(endpoint string) string {
	u := strings.TrimRight(endpoint, "/")
	if !strings.HasSuffix(u, "/api") {
		u += "/api"
	}
	return u + "/"
}

// Verify calls auth.test and checks the workspace matches cfg.AccountID.
func (a *Adapter) Verify(ctx context.Context) error {
	_, err := a.authTest(ctx)
	return err
}

func (a *Adapter) authTest(ctx context.Context) (*slack.AuthTestResponse, error) {
	resp, err := a.api.AuthTestContext(ctx)
	if err != nil {
		return nil, classify(err)
	}
	if a.cfg.AccountID != "" && resp.TeamID != a.cfg.AccountID {
		return nil, fmt.Errorf("%w: token belongs to workspace %s, expected %s",
			connectors.ErrUnauthorized, resp.TeamID, a.cfg.AccountID)
	}
	return resp, nil
}

// VerifyUser checks that userID is an active human in the workspace.
func (a *Adapter) VerifyUser(ctx context.Context, userID string) error {
	if userID == "" {
		return fmt.Errorf("%w: empty user id", connectors.ErrPermanent)
	}
	u, err := a.api.GetUserInfoContext(ctx, userID)
	if err != nil {
		return classify(err)
	}
	switch {
	case u.Deleted:
		return fmt.Errorf("%w: user %s is deactivated", connectors.ErrPermanent, userID)
	case u.IsBot:
		return fmt.Errorf("%w: user %s is a bot", connectors.ErrPermanent, userID)
	case a.cfg.AccountID != "" && u.TeamID != "" && u.TeamID != a.cfg.AccountID:
		return fmt.Errorf("%w: user %s belongs to workspace %s, expected %s",
			connectors.ErrPermanent, userID, u.TeamID, a.cfg.AccountID)
	}
	return nil
}

// Healthy reports whether the Socket Mode connection is established.
func (a *Adapter) Healthy() (bool, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.healthy, a.detail
}

func (a *Adapter) setHealth(ok bool, detail string) {
	a.mu.Lock()
	a.healthy, a.detail = ok, detail
	a.mu.Unlock()
}

// Run connects with Socket Mode and delivers DMs to sink until ctx is
// cancelled. Connection failures are retried with exponential backoff.
func (a *Adapter) Run(ctx context.Context, sink connectors.IngressSink) error {
	defer a.setHealth(false, "stopped")
	backoff := minBackoff
	for ctx.Err() == nil {
		connected, err := a.runOnce(ctx, sink)
		if ctx.Err() != nil {
			break
		}
		if connected {
			backoff = minBackoff
		}
		detail := "disconnected"
		if err != nil {
			detail = "disconnected: " + err.Error()
		}
		a.setHealth(false, detail)
		a.log.Warn("socket mode stopped; reconnecting", "error", err, "backoff", backoff)
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
		case <-t.C:
		}
		backoff = min(backoff*2, maxBackoff)
	}
	return nil
}

// runOnce authenticates, runs one socketmode client and returns when it
// stops. connected reports whether a websocket was established.
func (a *Adapter) runOnce(ctx context.Context, sink connectors.IngressSink) (bool, error) {
	auth, err := a.authTest(ctx)
	if err != nil {
		return false, err
	}
	smc := socketmode.New(a.api, socketmode.OptionPingInterval(a.pingInterval))
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	var up atomic.Bool
	wg.Go(func() {
		for {
			select {
			case <-runCtx.Done():
				return
			case evt := <-smc.Events:
				switch evt.Type {
				case socketmode.EventTypeConnected:
					up.Store(true)
					a.setHealth(true, "connected")
				case socketmode.EventTypeConnecting:
					a.setHealth(false, "connecting")
				case socketmode.EventTypeConnectionError:
					detail := "connection error"
					if ce, ok := evt.Data.(*slack.ConnectionErrorEvent); ok && ce.ErrorObj != nil {
						detail += ": " + ce.ErrorObj.Error()
					}
					a.setHealth(false, detail)
				case socketmode.EventTypeInvalidAuth:
					a.setHealth(false, "invalid auth")
				case socketmode.EventTypeDisconnect:
					a.setHealth(false, "disconnect requested")
				case socketmode.EventTypeEventsAPI:
					if evt.Request != nil {
						a.handleEnvelope(runCtx, smc, sink, auth.UserID, evt.Request)
					}
				}
			}
		}
	})
	err = smc.RunContext(runCtx)
	cancel()
	wg.Wait()
	if errors.Is(err, context.Canceled) {
		err = nil
	}
	return up.Load(), err
}

type callbackPayload struct {
	Type    string `json:"type"`
	TeamID  string `json:"team_id"`
	EventID string `json:"event_id"`
	Event   struct {
		Type        string `json:"type"`
		Subtype     string `json:"subtype"`
		BotID       string `json:"bot_id"`
		User        string `json:"user"`
		Channel     string `json:"channel"`
		ChannelType string `json:"channel_type"`
		Text        string `json:"text"`
		TS          string `json:"ts"`
		ThreadTS    string `json:"thread_ts"`
	} `json:"event"`
}

// handleEnvelope converts one events_api envelope. It acks events the adapter
// deliberately ignores, and acks a DM only after sink.Accept succeeds; when
// Accept fails the envelope is left unacked so Slack redelivers it.
func (a *Adapter) handleEnvelope(ctx context.Context, smc *socketmode.Client, sink connectors.IngressSink, botUserID string, req *socketmode.Request) {
	ack := func() {
		if err := smc.AckCtx(ctx, req.EnvelopeID, nil); err != nil {
			a.log.Warn("ack failed", "envelope_id", req.EnvelopeID, "error", err)
		}
	}
	ev, ok, reason := toInbound(req.Payload, botUserID, a.cfg.AccountID)
	if !ok {
		a.log.Debug("ignoring event", "envelope_id", req.EnvelopeID, "reason", reason)
		ack()
		return
	}
	if err := sink.Accept(ctx, a.cfg.Key, ev); err != nil {
		a.log.Warn("ingress rejected event; leaving unacked for redelivery",
			"event_id", ev.EventID, "retry_attempt", req.RetryAttempt, "error", err)
		return
	}
	ack()
}

// toInbound returns the InboundEvent for a human DM, or ok=false with a reason.
func toInbound(raw json.RawMessage, botUserID, accountID string) (connectors.InboundEvent, bool, string) {
	var p callbackPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return connectors.InboundEvent{}, false, "unparseable payload"
	}
	e := p.Event
	switch {
	case p.Type != "event_callback":
		return connectors.InboundEvent{}, false, "not an event callback"
	case e.Type != "message":
		return connectors.InboundEvent{}, false, "not a message"
	case e.ChannelType != "im":
		return connectors.InboundEvent{}, false, "not a direct message"
	case e.Subtype != "":
		return connectors.InboundEvent{}, false, "message subtype " + e.Subtype
	case e.BotID != "":
		return connectors.InboundEvent{}, false, "bot message"
	case e.User == "" || e.User == botUserID:
		return connectors.InboundEvent{}, false, "own or anonymous message"
	case p.EventID == "":
		return connectors.InboundEvent{}, false, "missing event id"
	case accountID != "" && p.TeamID != accountID:
		return connectors.InboundEvent{}, false, "foreign workspace " + p.TeamID
	}
	thread := e.Channel
	if e.ThreadTS != "" {
		thread = e.ThreadTS
	}
	return connectors.InboundEvent{
		EventID:   p.EventID,
		AccountID: p.TeamID,
		UserID:    e.User,
		ChannelID: e.Channel,
		ThreadRef: thread,
		Text:      e.Text,
		MessageTS: e.TS,
	}, true, ""
}

// Send posts msg as the bot. The DM channel is opened when only the user is
// known. The operation id travels as message metadata.
func (a *Adapter) Send(ctx context.Context, msg connectors.OutboundMessage) (string, error) {
	if strings.TrimSpace(msg.Text) == "" {
		return "", fmt.Errorf("%w: empty message text", connectors.ErrPermanent)
	}
	channel := msg.ChannelID
	if channel == "" {
		if msg.ExternalUserID == "" {
			return "", fmt.Errorf("%w: no channel or user to send to", connectors.ErrPermanent)
		}
		ch, _, _, err := a.api.OpenConversationContext(ctx, &slack.OpenConversationParameters{
			Users: []string{msg.ExternalUserID}, ReturnIM: true,
		})
		if err != nil {
			// Opening a DM has no side effect worth reconciling; an
			// ambiguous failure here is safe to retry.
			err = classify(err)
			if errors.Is(err, connectors.ErrAmbiguous) {
				err = fmt.Errorf("%w: conversations.open: %v", connectors.ErrRetryable, err)
			}
			return "", err
		}
		channel = ch.ID
	}
	opts := []slack.MsgOption{slack.MsgOptionText(msg.Text, false)}
	if isThreadTS(msg.ThreadRef) && msg.ThreadRef != channel {
		opts = append(opts, slack.MsgOptionTS(msg.ThreadRef))
	}
	if msg.IdempotencyKey != "" {
		opts = append(opts, slack.MsgOptionMetadata(slack.SlackMetadata{
			EventType:    MetadataEventType,
			EventPayload: map[string]any{"operation_id": msg.IdempotencyKey},
		}))
	}
	_, ts, err := a.api.PostMessageContext(ctx, channel, opts...)
	if err != nil {
		return "", classify(err)
	}
	return ts, nil
}

// isThreadTS reports whether ref looks like a Slack message timestamp.
func isThreadTS(ref string) bool {
	head, tail, ok := strings.Cut(ref, ".")
	return ok && head != "" && tail != "" && strings.Trim(head+tail, "0123456789") == ""
}

// classify maps a slack-go error to a connector outcome.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var rl *slack.RateLimitedError
	if errors.As(err, &rl) {
		return fmt.Errorf("%w: %v", connectors.ErrRetryable, err)
	}
	var sc slack.StatusCodeError
	if errors.As(err, &sc) {
		return httpx.ClassifyStatus(sc.Code, sc.Status)
	}
	var se slack.SlackErrorResponse
	if errors.As(err, &se) {
		return classifyCode(se.Err, err)
	}
	var sent *httpx.SentError
	if errors.As(err, &sent) {
		return httpx.ClassifyTransport(err)
	}
	if code := err.Error(); isSlackCode(code) {
		return classifyCode(code, err)
	}
	return httpx.ClassifyTransport(err)
}

func classifyCode(code string, err error) error {
	switch code {
	case "ratelimited", "rate_limited":
		return fmt.Errorf("%w: %v", connectors.ErrRetryable, err)
	case "invalid_auth", "not_authed", "token_revoked", "token_expired", "account_inactive", "missing_scope", "not_allowed_token_type":
		return fmt.Errorf("%w: %v", connectors.ErrUnauthorized, err)
	case "internal_error", "fatal_error", "service_unavailable", "request_timeout":
		return fmt.Errorf("%w: %v", connectors.ErrAmbiguous, err)
	}
	return fmt.Errorf("%w: %v", connectors.ErrPermanent, err)
}

// isSlackCode reports whether s looks like a bare Slack error code
// (slack-go returns some API errors as errors.New(code)).
func isSlackCode(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && r != '_' {
			return false
		}
	}
	return true
}
