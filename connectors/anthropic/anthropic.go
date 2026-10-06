// Package anthropic is the model-serving adapter. The platform proxies
// Anthropic API calls for seats and injects the credential, so the key never
// enters a sandbox (§5.2, ADR-0001).
package anthropic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/connectors/internal/httpx"
)

// DefaultEndpoint is the Anthropic API base URL.
const DefaultEndpoint = "https://api.anthropic.com"

// DefaultAPIVersion is sent by Verify and by proxied requests that set none.
const DefaultAPIVersion = "2023-06-01"

// DefaultMaxRequestBytes bounds proxied request bodies (Anthropic's own limit
// for the Messages API is 32 MB).
const DefaultMaxRequestBytes int64 = 32 << 20

// strippedHeaders never reach the provider: caller credentials and platform
// correlation headers.
var strippedHeaders = []string{
	"Authorization", "X-Api-Key", "Cookie", "Proxy-Authorization",
	"X-Steadmesh-Generation", "X-Steadmesh-Execution",
}

// Adapter implements connectors.Model for Anthropic.
type Adapter struct {
	base     *url.URL
	apiKey   string
	maxBytes int64
	client   *http.Client
	proxy    *httputil.ReverseProxy
}

var _ connectors.Model = (*Adapter)(nil)

// New builds the adapter. cfg.Secret["api_key"] is required. cfg.Endpoint
// overrides the base URL; cfg.Extra["max_request_bytes"] overrides the body cap.
func New(cfg connectors.Config) (connectors.Model, error) {
	return newAdapter(cfg)
}

func newAdapter(cfg connectors.Config) (*Adapter, error) {
	key := cfg.Secret["api_key"]
	if key == "" {
		return nil, fmt.Errorf("%w: anthropic secret must contain api_key", connectors.ErrUnauthorized)
	}
	ep := cfg.Endpoint
	if ep == "" {
		ep = DefaultEndpoint
	}
	base, err := url.Parse(ep)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("%w: invalid anthropic endpoint %q", connectors.ErrPermanent, ep)
	}
	a := &Adapter{base: base, apiKey: key, maxBytes: DefaultMaxRequestBytes, client: httpx.Client(cfg.HTTP)}
	if v := cfg.Extra["max_request_bytes"]; v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("%w: invalid max_request_bytes %q", connectors.ErrPermanent, v)
		}
		a.maxBytes = n
	}
	log := slog.Default().With("connector", "anthropic", "connection", cfg.Key)
	a.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(a.base)
			pr.Out.Host = a.base.Host
			for _, h := range strippedHeaders {
				pr.Out.Header.Del(h)
			}
			pr.Out.Header.Set("X-Api-Key", a.apiKey)
			if pr.Out.Header.Get("Anthropic-Version") == "" {
				pr.Out.Header.Set("Anthropic-Version", DefaultAPIVersion)
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

// Verify lists models with the configured key.
func (a *Adapter) Verify(ctx context.Context) error {
	u := a.base.JoinPath("/v1/models")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("%w: %v", connectors.ErrPermanent, err)
	}
	req.Header.Set("X-Api-Key", a.apiKey)
	req.Header.Set("Anthropic-Version", DefaultAPIVersion)
	resp, err := a.client.Do(req)
	if err != nil {
		// A read-only probe: any transport failure is retryable.
		return fmt.Errorf("%w: %v", connectors.ErrRetryable, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		err := httpx.ClassifyStatus(resp.StatusCode, "GET /v1/models")
		if errors.Is(err, connectors.ErrAmbiguous) {
			err = fmt.Errorf("%w: %v", connectors.ErrRetryable, err)
		}
		return err
	}
	return nil
}

// Proxy returns the credential-injecting reverse proxy. The request path must
// already have the platform prefix removed (e.g. /v1/messages).
func (a *Adapter) Proxy() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
