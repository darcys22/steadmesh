// Package egress is the egress gateway: an HTTP proxy that lets seats reach
// the hosts their access profiles allow, and nothing else (docs/sandbox.html).
//
// Seats reach it only through the local proxy in their seat runner, which
// adds Proxy-Authorization: Bearer <projected seat token> to every
// connection. The gateway asks the platform for the seat's current access
// (GET /v1/access with that token), so it holds no credentials and no
// Kubernetes rights, and identity never depends on Pod IPs.
//
//   - CONNECT host:port is allowed only when a rule matches host and port;
//     the TLS ClientHello's server name must then equal the CONNECT host, so
//     an allowed name cannot be used to reach another TLS server.
//   - Plain HTTP requests are matched on their host.
//   - Open tunnels are re-checked every few seconds and closed as soon as
//     their rule is gone or the seat's token stops authenticating.
//   - Denials answer 403 naming the host, and are recorded as egress_denied
//     events on the seat's execution.
package egress

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/darcys22/steadmesh/pkg/access"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

// Gateway is the egress proxy.
type Gateway struct {
	// PlatformURL is the platform base URL (access lookups and events).
	PlatformURL string
	// HTTP calls the platform. Dial dials upstream hosts.
	HTTP *http.Client
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// CacheTTL bounds how long a seat's access is reused; Recheck is how
	// often open tunnels are re-authorised.
	CacheTTL time.Duration
	Recheck  time.Duration
	// HelloTimeout bounds the wait for a TLS ClientHello.
	HelloTimeout time.Duration
	Log          *slog.Logger

	once    sync.Once
	mu      sync.Mutex
	cache   map[[32]byte]cached
	tunnels map[*tunnel]struct{}
}

type cached struct {
	at  time.Time
	acc *runtimeapi.AccessResponse
	err error
}

type tunnel struct {
	token      string
	host       string
	port       int
	seat       string
	exec, gen  string
	client, up net.Conn
	closeOnce  sync.Once
}

func (t *tunnel) close() {
	t.closeOnce.Do(func() {
		_ = t.client.Close()
		if t.up != nil {
			_ = t.up.Close()
		}
	})
}

// errUnauthenticated means the platform rejected the seat token.
var errUnauthenticated = errors.New("seat token not accepted")

func (g *Gateway) defaults() { g.once.Do(g.setDefaults) }

func (g *Gateway) setDefaults() {
	if g.HTTP == nil {
		g.HTTP = &http.Client{Timeout: 10 * time.Second}
	}
	if g.Dial == nil {
		d := &net.Dialer{Timeout: 10 * time.Second}
		g.Dial = d.DialContext
	}
	if g.CacheTTL == 0 {
		g.CacheTTL = 2 * time.Second
	}
	if g.Recheck == 0 {
		g.Recheck = 3 * time.Second
	}
	if g.HelloTimeout == 0 {
		g.HelloTimeout = 10 * time.Second
	}
	if g.Log == nil {
		g.Log = slog.Default()
	}
}

// Run re-checks open tunnels until ctx ends.
func (g *Gateway) Run(ctx context.Context) {
	g.defaults()
	t := time.NewTicker(g.Recheck)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			g.mu.Lock()
			for tn := range g.tunnels {
				tn.close()
			}
			g.mu.Unlock()
			return
		case <-t.C:
			g.recheck(ctx)
		}
	}
}

// recheck closes tunnels whose access is gone.
func (g *Gateway) recheck(ctx context.Context) {
	g.mu.Lock()
	open := make([]*tunnel, 0, len(g.tunnels))
	for tn := range g.tunnels {
		open = append(open, tn)
	}
	g.mu.Unlock()
	for _, tn := range open {
		acc, err := g.access(ctx, tn.token)
		reason := ""
		switch {
		case errors.Is(err, errUnauthenticated):
			reason = "the seat's identity is no longer valid"
		case err != nil:
			continue // platform unreachable: keep existing tunnels, deny new ones
		default:
			if _, ok := allows(acc, tn.host, tn.port); !ok {
				reason = "access was revoked"
			}
		}
		if reason == "" {
			continue
		}
		g.Log.Info("closing tunnel", "seat", tn.seat, "host", tn.host, "port", tn.port, "reason", reason)
		tn.close()
		g.event(tn.token, tn.gen, tn.exec, runtimeapi.EventEgressRevoked, map[string]any{"host": tn.host, "port": tn.port, "reason": reason})
	}
}

func allows(acc *runtimeapi.AccessResponse, host string, port int) (access.EgressRule, bool) {
	a := access.SeatAccess{Egress: acc.Egress}
	return a.Allows(host, port)
}

// access returns the seat's access for a token, from the cache when fresh.
func (g *Gateway) access(ctx context.Context, token string) (*runtimeapi.AccessResponse, error) {
	key := sha256.Sum256([]byte(token))
	g.mu.Lock()
	c, ok := g.cache[key]
	g.mu.Unlock()
	if ok && time.Since(c.at) < g.CacheTTL {
		return c.acc, c.err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(g.PlatformURL, "/")+runtimeapi.PathAccess, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var acc runtimeapi.AccessResponse
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		err = errUnauthenticated
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("platform access lookup: %s", resp.Status)
	default:
		if err := json.NewDecoder(resp.Body).Decode(&acc); err != nil {
			return nil, err
		}
	}
	g.mu.Lock()
	if g.cache == nil {
		g.cache = map[[32]byte]cached{}
	}
	if len(g.cache) > 10000 {
		g.cache = map[[32]byte]cached{}
	}
	g.cache[key] = cached{at: time.Now(), acc: &acc, err: err}
	g.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &acc, nil
}

// event records a sandbox access event on the seat's execution, using the
// seat's own token. It is best effort and never blocks the request.
func (g *Gateway) event(token, gen, exec, kind string, data map[string]any) {
	if exec == "" {
		return
	}
	go func() {
		b, _ := json.Marshal([]runtimeapi.ExecutionEvent{{Kind: kind, Time: time.Now().UTC(), Data: mustJSON(data)}})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		u := strings.TrimSuffix(g.PlatformURL, "/") + runtimeapi.PathExecEvents + url.PathEscape(exec) + "/events"
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
		if err != nil {
			return
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		if gen != "" {
			req.Header.Set(runtimeapi.HeaderGeneration, gen)
		}
		if resp, err := g.HTTP.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func bearer(h string) string {
	tok, ok := strings.CutPrefix(h, "Bearer ")
	if !ok {
		return ""
	}
	return strings.TrimSpace(tok)
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.defaults()
	if r.Method != http.MethodConnect && r.URL.Path == "/healthz" && r.URL.Host == "" {
		_, _ = io.WriteString(w, "ok")
		return
	}
	token := bearer(r.Header.Get("Proxy-Authorization"))
	if token == "" {
		w.Header().Set("Proxy-Authenticate", `Bearer realm="steadmesh"`)
		http.Error(w, "steadmesh egress: a seat token is required", http.StatusProxyAuthRequired)
		return
	}
	gen, exec := r.Header.Get(runtimeapi.HeaderGeneration), r.Header.Get(runtimeapi.HeaderExecution)
	acc, err := g.access(r.Context(), token)
	switch {
	case errors.Is(err, errUnauthenticated):
		http.Error(w, "steadmesh egress: the seat token is not valid", http.StatusProxyAuthRequired)
		return
	case err != nil:
		g.Log.Warn("access lookup failed", "err", err)
		http.Error(w, "steadmesh egress: the platform is unavailable; try again", http.StatusServiceUnavailable)
		return
	}
	host, port, err := target(r)
	if err != nil {
		http.Error(w, "steadmesh egress: "+err.Error(), http.StatusBadRequest)
		return
	}
	if _, ok := allows(acc, host, port); !ok {
		msg := fmt.Sprintf("steadmesh egress: %s:%d is not allowed for seat %s; no access profile grants it", host, port, acc.SeatKey)
		g.Log.Info("egress denied", "seat", acc.SeatKey, "host", host, "port", port)
		g.event(token, gen, exec, runtimeapi.EventEgressDenied, map[string]any{"host": host, "port": port, "reason": "no access profile grants it"})
		http.Error(w, msg, http.StatusForbidden)
		return
	}
	if r.Method == http.MethodConnect {
		g.connect(w, r, &tunnel{token: token, host: host, port: port, seat: acc.SeatKey, exec: exec, gen: gen})
		return
	}
	g.forward(w, r)
}

// target is the request's destination host and port.
func target(r *http.Request) (string, int, error) {
	hostport := r.Host
	if r.Method != http.MethodConnect {
		if r.URL.Host == "" {
			return "", 0, errors.New("only absolute-form proxy requests are accepted")
		}
		if r.URL.Scheme != "http" {
			return "", 0, fmt.Errorf("use CONNECT for %s", r.URL.Scheme)
		}
		hostport = r.URL.Host
	}
	h, p, err := net.SplitHostPort(hostport)
	if err != nil {
		if r.Method == http.MethodConnect {
			return "", 0, fmt.Errorf("CONNECT needs host:port, got %q", hostport)
		}
		h, p = hostport, "80"
	}
	port, err := strconv.Atoi(p)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("invalid port %q", p)
	}
	return strings.ToLower(strings.TrimSuffix(h, ".")), port, nil
}

// connect establishes a tunnel after checking the TLS server name.
func (g *Gateway) connect(w http.ResponseWriter, r *http.Request, tn *tunnel) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "steadmesh egress: tunnelling unsupported", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	tn.client = client
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		client.Close()
		return
	}
	// Read the first client bytes: a TLS ClientHello's server name must be
	// the CONNECT host. Plain protocols are allowed only off port 443.
	_ = client.SetReadDeadline(time.Now().Add(g.HelloTimeout))
	first, sni, isTLS := readHello(buf.Reader)
	_ = client.SetReadDeadline(time.Time{})
	if isTLS && net.ParseIP(tn.host) == nil && !strings.EqualFold(sni, tn.host) {
		g.Log.Info("egress denied: server name mismatch", "seat", tn.seat, "host", tn.host, "sni", sni)
		g.event(tn.token, tn.gen, tn.exec, runtimeapi.EventEgressDenied, map[string]any{"host": tn.host, "port": tn.port, "reason": fmt.Sprintf("TLS server name %q does not match the CONNECT host", sni)})
		client.Close()
		return
	}
	if !isTLS && tn.port == 443 {
		g.event(tn.token, tn.gen, tn.exec, runtimeapi.EventEgressDenied, map[string]any{"host": tn.host, "port": tn.port, "reason": "port 443 carries only TLS"})
		client.Close()
		return
	}
	up, err := g.Dial(r.Context(), "tcp", net.JoinHostPort(tn.host, strconv.Itoa(tn.port)))
	if err != nil {
		g.Log.Info("upstream dial failed", "host", tn.host, "port", tn.port, "err", err)
		client.Close()
		return
	}
	tn.up = up
	if _, err := up.Write(first); err != nil {
		tn.close()
		return
	}
	g.mu.Lock()
	if g.tunnels == nil {
		g.tunnels = map[*tunnel]struct{}{}
	}
	g.tunnels[tn] = struct{}{}
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.tunnels, tn)
		g.mu.Unlock()
		tn.close()
	}()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, buf.Reader); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, up); done <- struct{}{} }()
	<-done
}

// OpenTunnels counts open tunnels.
func (g *Gateway) OpenTunnels() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.tunnels)
}

// readHello reads the client's first bytes. It reports the TLS server name
// when they are a ClientHello, and returns every byte read for replay.
func readHello(r *bufio.Reader) (first []byte, sni string, isTLS bool) {
	b, err := r.Peek(1)
	if err != nil || b[0] != 0x16 {
		n := r.Buffered()
		if n == 0 && err == nil {
			n = 1
		}
		first = make([]byte, n)
		k, _ := io.ReadFull(r, first)
		return first[:k], "", false
	}
	var rec recorder
	rec.r = r
	srv := tls.Server(&rec, &tls.Config{GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
		sni = h.ServerName
		return nil, errHelloRead
	}})
	_ = srv.Handshake()
	return rec.buf.Bytes(), sni, true
}

var errHelloRead = errors.New("client hello read")

// recorder is a net.Conn that records what tls reads and discards writes.
type recorder struct {
	net.Conn
	r   io.Reader
	buf bytes.Buffer
}

func (c *recorder) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.buf.Write(p[:n])
	return n, err
}
func (c *recorder) Write(p []byte) (int, error)      { return len(p), nil }
func (c *recorder) Close() error                     { return nil }
func (c *recorder) SetDeadline(time.Time) error      { return nil }
func (c *recorder) SetReadDeadline(time.Time) error  { return nil }
func (c *recorder) SetWriteDeadline(time.Time) error { return nil }
func (c *recorder) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *recorder) RemoteAddr() net.Addr             { return &net.TCPAddr{} }

// forward proxies a plain HTTP request.
func (g *Gateway) forward(w http.ResponseWriter, r *http.Request) {
	out := r.Clone(r.Context())
	out.RequestURI = ""
	for _, h := range []string{"Proxy-Authorization", "Proxy-Connection", runtimeapi.HeaderGeneration, runtimeapi.HeaderExecution} {
		out.Header.Del(h)
	}
	tr := &http.Transport{DialContext: g.Dial, Proxy: nil, DisableCompression: true, ResponseHeaderTimeout: 2 * time.Minute}
	defer tr.CloseIdleConnections()
	resp, err := tr.RoundTrip(out)
	if err != nil {
		http.Error(w, "steadmesh egress: upstream unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
