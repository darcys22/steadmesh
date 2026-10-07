// Package model is the model-serving connector for the anthropic, openai and
// model adapters. The platform proxies seats' model requests through it and
// injects the credential, so no key enters a sandbox (§5.2, ADR-0001).
//
// A connection's endpoint is its API base, usually ending in /v1
// (https://api.openai.com/v1). Seats address the proxy with the API path
// below /v1, e.g. /v1/messages, /v1/responses or /v1/chat/completions, which
// is joined to the base.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/connectors/internal/httpx"
	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/pkg/spec"
)

// Preset endpoints.
const (
	AnthropicEndpoint = "https://api.anthropic.com/v1"
	OpenAIEndpoint    = "https://api.openai.com/v1"
)

// DefaultAnthropicVersion is sent on Anthropic requests that set none.
const DefaultAnthropicVersion = "2023-06-01"

// DefaultMaxRequestBytes bounds proxied request bodies.
const DefaultMaxRequestBytes int64 = 32 << 20

// ProbeTTL is how long a successful request probe is trusted. Probes are
// real (one-token) requests, so they are not repeated on every check.
const ProbeTTL = 6 * time.Hour

// strippedHeaders never reach the provider: caller credentials and platform
// correlation headers.
var strippedHeaders = []string{
	"Authorization", "X-Api-Key", "Cookie", "Proxy-Authorization",
	"X-Steadmesh-Generation", "X-Steadmesh-Execution",
}

// presets are the defaults of the named adapters.
var presets = map[string]struct {
	endpoint string
	apis     []string
	auth     string
}{
	"anthropic": {AnthropicEndpoint, []string{harnesses.APIAnthropicMessages}, "x-api-key"},
	"openai":    {OpenAIEndpoint, []string{harnesses.APIOpenAIResponses, harnesses.APIOpenAIChat}, "bearer"},
	"model":     {"", nil, "bearer"},
}

// APIForPath returns the API a proxied request path belongs to: one of the
// harnesses.API* constants, "models" for a model listing, or "" when the
// path is not a model API.
func APIForPath(path string) string {
	p := strings.TrimSuffix(path, "/")
	switch {
	case p == "/v1/messages" || p == "/v1/messages/count_tokens":
		return harnesses.APIAnthropicMessages
	case p == "/v1/responses" || strings.HasPrefix(p, "/v1/responses/"):
		return harnesses.APIOpenAIResponses
	case p == "/v1/chat/completions":
		return harnesses.APIOpenAIChat
	case p == "/v1/models" || strings.HasPrefix(p, "/v1/models/"):
		return "models"
	}
	return ""
}

// Adapter implements connectors.Model.
type Adapter struct {
	key      string
	base     *url.URL
	auth     string
	secret   string
	ep       spec.ModelEndpoint
	uses     []connectors.ModelUse
	maxBytes int64
	client   *http.Client
	proxy    *httputil.ReverseProxy

	mu     sync.Mutex
	probed map[string]time.Time // probe key -> success time
	now    func() time.Time
}

var _ connectors.Model = (*Adapter)(nil)

// New builds the adapter. cfg.Secret["api_key"] is required; cfg.Endpoint
// overrides the preset base URL; cfg.Model describes the endpoint;
// cfg.Extra["max_request_bytes"] overrides the body cap.
func New(cfg connectors.Config) (connectors.Model, error) { return newAdapter(cfg) }

func newAdapter(cfg connectors.Config) (*Adapter, error) {
	pre, ok := presets[cfg.Adapter]
	if !ok {
		return nil, fmt.Errorf("%w: model connector does not handle adapter %q", connectors.ErrPermanent, cfg.Adapter)
	}
	key := cfg.Secret["api_key"]
	if key == "" {
		return nil, fmt.Errorf("%w: %s secret must contain api_key", connectors.ErrUnauthorized, cfg.Adapter)
	}
	ep := cfg.Endpoint
	if ep == "" {
		ep = pre.endpoint
	}
	base, err := url.Parse(strings.TrimSuffix(ep, "/"))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("%w: invalid model endpoint %q", connectors.ErrPermanent, ep)
	}
	a := &Adapter{key: cfg.Key, base: base, secret: key, uses: cfg.ModelUses, maxBytes: DefaultMaxRequestBytes, client: httpx.Client(cfg.HTTP),
		probed: map[string]time.Time{}, now: time.Now}
	if cfg.Model != nil {
		a.ep = *cfg.Model
	}
	if len(a.ep.APIs) == 0 {
		a.ep.APIs = pre.apis
	}
	if a.ep.Auth == "" {
		a.ep.Auth = pre.auth
	}
	if a.ep.Verify == "" {
		a.ep.Verify = "request"
	}
	a.auth = a.ep.Auth
	if a.auth != "bearer" && a.auth != "x-api-key" && !strings.HasPrefix(a.auth, "header:") {
		return nil, fmt.Errorf("%w: invalid auth %q", connectors.ErrPermanent, a.auth)
	}
	if v := cfg.Extra["max_request_bytes"]; v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("%w: invalid max_request_bytes %q", connectors.ErrPermanent, v)
		}
		a.maxBytes = n
	}
	log := slog.Default().With("connector", cfg.Adapter, "connection", cfg.Key)
	a.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			u := *a.base
			u.Path = a.base.Path + strings.TrimPrefix(pr.In.URL.Path, "/v1")
			u.RawPath = ""
			u.RawQuery = pr.In.URL.RawQuery
			pr.Out.URL = &u
			pr.Out.Host = a.base.Host
			for _, h := range strippedHeaders {
				pr.Out.Header.Del(h)
			}
			a.setAuth(pr.Out.Header)
			if APIForPath(pr.In.URL.Path) == harnesses.APIAnthropicMessages && pr.Out.Header.Get("Anthropic-Version") == "" {
				pr.Out.Header.Set("Anthropic-Version", DefaultAnthropicVersion)
			}
		},
		// Flush immediately so server-sent events stream through.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			if errors.Is(err, context.Canceled) {
				return
			}
			log.Warn("model proxy upstream error", "path", r.URL.Path, "error", err)
			http.Error(w, "model provider unavailable", http.StatusBadGateway)
		},
	}
	if cfg.HTTP != nil && cfg.HTTP.Transport != nil {
		a.proxy.Transport = cfg.HTTP.Transport
	}
	return a, nil
}

func (a *Adapter) setAuth(h http.Header) {
	switch {
	case a.auth == "x-api-key":
		h.Set("X-Api-Key", a.secret)
	case strings.HasPrefix(a.auth, "header:"):
		h.Set(strings.TrimPrefix(a.auth, "header:"), a.secret)
	default:
		h.Set("Authorization", "Bearer "+a.secret)
	}
}

// Host is the upstream host, recorded with each proxied request.
func (a *Adapter) Host() string { return a.base.Host }

// Serves reports whether the connection serves api.
func (a *Adapter) Serves(api string) bool {
	for _, x := range a.ep.APIs {
		if x == api {
			return true
		}
	}
	return false
}

// Proxy returns the credential-injecting reverse proxy. The request path is
// the API path, e.g. /v1/messages.
func (a *Adapter) Proxy() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/") {
			http.Error(w, "model API paths start with /v1/", http.StatusNotFound)
			return
		}
		if r.ContentLength > a.maxBytes {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, a.maxBytes)
		}
		a.proxy.ServeHTTP(w, r)
	})
}

// probe is one readiness check of an API and model.
type probe struct{ api, model string }

// probes lists the API and model pairs the connection claims and the ones
// harness profiles use.
func (a *Adapter) probes() []probe {
	var out []probe
	seen := map[probe]bool{}
	add := func(p probe) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, m := range a.ep.Models {
		apis := m.APIs
		if len(apis) == 0 {
			apis = a.ep.APIs
		}
		for _, api := range apis {
			add(probe{api, m.ID})
		}
	}
	for _, u := range a.uses {
		add(probe{u.API, u.ID})
	}
	return out
}

// Verify checks the credential and the endpoint's claims (spec verify mode):
// request sends a minimal request for each API and model (results are
// trusted for ProbeTTL), models lists models, none checks nothing remote.
// A failure names the API and model.
func (a *Adapter) Verify(ctx context.Context) error {
	switch a.ep.Verify {
	case "none":
		return nil
	case "models":
		return a.listModels(ctx)
	}
	ps := a.probes()
	if len(ps) == 0 {
		return a.listModels(ctx)
	}
	for _, p := range ps {
		k := p.api + "\x00" + p.model
		a.mu.Lock()
		at, ok := a.probed[k]
		a.mu.Unlock()
		if ok && a.now().Sub(at) < ProbeTTL {
			continue
		}
		if err := a.probe(ctx, p); err != nil {
			return err
		}
		a.mu.Lock()
		a.probed[k] = a.now()
		a.mu.Unlock()
	}
	return nil
}

func (a *Adapter) listModels(ctx context.Context) error {
	_, err := a.do(ctx, http.MethodGet, "/models", nil, "GET models")
	return err
}

func (a *Adapter) probe(ctx context.Context, p probe) error {
	what := fmt.Sprintf("%s model %q", p.api, p.model)
	var path string
	var body map[string]any
	switch p.api {
	case harnesses.APIAnthropicMessages:
		path, body = "/messages", map[string]any{"model": p.model, "max_tokens": 1, "messages": []any{map[string]any{"role": "user", "content": "ping"}}}
	case harnesses.APIOpenAIResponses:
		path, body = "/responses", map[string]any{"model": p.model, "input": "ping", "max_output_tokens": 16, "store": false}
	case harnesses.APIOpenAIChat:
		path, body = "/chat/completions", map[string]any{"model": p.model, "messages": []any{map[string]any{"role": "user", "content": "ping"}}, "max_completion_tokens": 1}
		_, err := a.do(ctx, http.MethodPost, path, body, what)
		if err == nil || !errors.Is(err, connectors.ErrPermanent) || !strings.Contains(err.Error(), "max_completion_tokens") {
			return err
		}
		// Older Chat Completions servers only know max_tokens.
		delete(body, "max_completion_tokens")
		body["max_tokens"] = 1
	default:
		return fmt.Errorf("%w: unknown API %q", connectors.ErrPermanent, p.api)
	}
	_, err := a.do(ctx, http.MethodPost, path, body, what)
	return err
}

// do sends a request to the endpoint and classifies the outcome.
func (a *Adapter) do(ctx context.Context, method, path string, body any, what string) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base.String()+path, rd)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", connectors.ErrPermanent, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	a.setAuth(req.Header)
	if strings.HasPrefix(path, "/messages") || a.Serves(harnesses.APIAnthropicMessages) {
		req.Header.Set("Anthropic-Version", DefaultAnthropicVersion)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		// A probe has no effect worth protecting: any transport failure is retryable.
		return nil, fmt.Errorf("%w: %s: %v", connectors.ErrRetryable, what, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		detail := what + ": " + strings.TrimSpace(harnesses.TruncateUTF8(string(b), 300))
		err := httpx.ClassifyStatus(resp.StatusCode, detail)
		if errors.Is(err, connectors.ErrAmbiguous) {
			err = fmt.Errorf("%w: %v", connectors.ErrRetryable, err)
		}
		return b, err
	}
	return b, nil
}
