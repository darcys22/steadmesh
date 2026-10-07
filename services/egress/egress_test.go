package egress

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/pkg/access"
	"github.com/darcys22/steadmesh/pkg/egressforward"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

// platform is a stub of the platform's access and events endpoints.
type platform struct {
	mu     sync.Mutex
	token  string
	rules  []access.EgressRule
	events []runtimeapi.ExecutionEvent
	execs  []string
}

func (p *platform) set(rules ...access.EgressRule) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = rules
}

func (p *platform) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+p.token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case r.URL.Path == runtimeapi.PathAccess:
		_ = json.NewEncoder(w).Encode(runtimeapi.AccessResponse{SeatKey: "engineer", Egress: p.rules})
	case strings.HasPrefix(r.URL.Path, runtimeapi.PathExecEvents):
		var evs []runtimeapi.ExecutionEvent
		_ = json.NewDecoder(r.Body).Decode(&evs)
		p.events = append(p.events, evs...)
		p.execs = append(p.execs, strings.Split(strings.TrimPrefix(r.URL.Path, runtimeapi.PathExecEvents), "/")[0])
		w.WriteHeader(http.StatusNoContent)
	}
}

func (p *platform) eventKinds() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, e := range p.events {
		out = append(out, e.Kind+":"+string(e.Data))
	}
	return out
}

type rig struct {
	p        *platform
	g        *Gateway
	fwd      *egressforward.Forwarder
	upstream *httptest.Server
	plain    *httptest.Server
	tokFile  string
}

// newRig starts upstream servers that names resolve to: allowed.test:443 and
// other.test:443 reach the TLS server, plain.test:80 the HTTP server.
func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{p: &platform{token: "seat-tok"}}
	r.upstream = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprintf(w, "hello %s", req.Host)
	}))
	t.Cleanup(r.upstream.Close)
	r.plain = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprintf(w, "plain %s auth=%q", req.Host, req.Header.Get("Proxy-Authorization"))
	}))
	t.Cleanup(r.plain.Close)
	ps := httptest.NewServer(r.p)
	t.Cleanup(ps.Close)
	hosts := map[string]string{
		"allowed.test:443": strings.TrimPrefix(r.upstream.URL, "https://"),
		"other.test:443":   strings.TrimPrefix(r.upstream.URL, "https://"),
		"plain.test:80":    strings.TrimPrefix(r.plain.URL, "http://"),
		"plain.test:443":   strings.TrimPrefix(r.plain.URL, "http://"),
	}
	r.g = &Gateway{PlatformURL: ps.URL, CacheTTL: 20 * time.Millisecond, Recheck: 50 * time.Millisecond, HelloTimeout: 2 * time.Second,
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if a, ok := hosts[addr]; ok {
				addr = a
			}
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}}
	gs := httptest.NewServer(r.g)
	t.Cleanup(gs.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go r.g.Run(ctx)
	r.tokFile = filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(r.tokFile, []byte("seat-tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	fwd, err := egressforward.Start(egressforward.Options{GatewayURL: gs.URL, TokenFile: r.tokFile,
		Generation: func() int64 { return 7 }, Execution: func() string { return "exec-1" }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fwd.Close)
	r.fwd = fwd
	return r
}

func (r *rig) client() *http.Client {
	proxy, _ := url.Parse(r.fwd.URL())
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxy),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test upstream certificate
	}}
}

func rule(host string, ports ...int) access.EgressRule {
	return access.EgressRule{Host: host, Ports: ports, Source: "access_profile:test"}
}

func TestAllowedHTTPSAndHTTP(t *testing.T) {
	r := newRig(t)
	r.p.set(rule("allowed.test", 443), rule("plain.test", 80))
	resp, err := r.client().Get("https://allowed.test/x")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "hello allowed.test" {
		t.Fatalf("body %q", b)
	}
	resp, err = r.client().Get("http://plain.test/y")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	// The seat token never reaches the destination.
	if string(b) != `plain plain.test auth=""` {
		t.Fatalf("plain body %q", b)
	}
}

func TestDeniedHostIsForbiddenAndRecorded(t *testing.T) {
	r := newRig(t)
	r.p.set(rule("allowed.test", 443))
	if _, err := r.client().Get("https://other.test/"); err == nil || !strings.Contains(err.Error(), "Forbidden") {
		t.Fatalf("CONNECT to an ungranted host: %v", err)
	}
	resp, err := r.client().Get("http://plain.test/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(b), "plain.test:80 is not allowed for seat engineer") {
		t.Fatalf("plain denial: %d %s", resp.StatusCode, b)
	}
	waitFor(t, func() bool { return len(r.p.eventKinds()) == 2 })
	for _, e := range r.p.eventKinds() {
		if !strings.HasPrefix(e, runtimeapi.EventEgressDenied+":") || !strings.Contains(e, ".test") {
			t.Fatalf("events %v", r.p.eventKinds())
		}
	}
	if r.p.execs[0] != "exec-1" {
		t.Fatalf("event execution %v", r.p.execs)
	}
}

func TestServerNameMustMatchConnectHost(t *testing.T) {
	r := newRig(t)
	r.p.set(rule("allowed.test", 443), rule("plain.test", 443))
	// CONNECT allowed.test, then a TLS handshake naming another server.
	conn := r.tunnel(t, "allowed.test:443")
	tc := tls.Client(conn, &tls.Config{ServerName: "other.test", InsecureSkipVerify: true}) //nolint:gosec
	_ = tc.SetDeadline(time.Now().Add(3 * time.Second))
	if err := tc.Handshake(); err == nil {
		t.Fatal("handshake with a different server name succeeded")
	}
	waitFor(t, func() bool {
		return strings.Contains(strings.Join(r.p.eventKinds(), " "), "does not match the CONNECT host")
	})
	// Port 443 carries only TLS.
	conn = r.tunnel(t, "plain.test:443")
	_, _ = conn.Write([]byte("GET / HTTP/1.1\r\nHost: plain.test\r\n\r\n"))
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if n, _ := conn.Read(make([]byte, 64)); n > 0 {
		t.Fatal("plain HTTP passed over port 443")
	}
}

// tunnel opens a CONNECT tunnel through the local forwarder.
func (r *rig) tunnel(t *testing.T, hostport string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(r.fwd.URL(), "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", hostport, hostport)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT %s: %v %v", hostport, resp, err)
	}
	return c
}

func TestRevocationClosesOpenTunnels(t *testing.T) {
	r := newRig(t)
	r.p.set(rule("allowed.test", 443))
	conn := r.tunnel(t, "allowed.test:443")
	tc := tls.Client(conn, &tls.Config{ServerName: "allowed.test", InsecureSkipVerify: true}) //nolint:gosec
	if err := tc.Handshake(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return r.g.OpenTunnels() == 1 })
	r.p.set() // the profile is removed from the seat
	start := time.Now()
	_ = tc.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := tc.Read(make([]byte, 1)); err == nil {
		t.Fatal("tunnel still open after revocation")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("tunnel closed after %v", time.Since(start))
	}
	waitFor(t, func() bool {
		return strings.Contains(strings.Join(r.p.eventKinds(), " "), runtimeapi.EventEgressRevoked)
	})
	waitFor(t, func() bool { return r.g.OpenTunnels() == 0 })
}

func TestTokenRequired(t *testing.T) {
	r := newRig(t)
	r.p.set(rule("allowed.test", 443))
	if err := os.WriteFile(r.tokFile, []byte("someone-else"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.g.mu.Lock()
	r.g.cache = nil
	r.g.mu.Unlock()
	if _, err := r.client().Get("https://allowed.test/"); err == nil || !strings.Contains(err.Error(), "Proxy Authentication Required") {
		t.Fatalf("invalid token: %v", err)
	}
	// A rotated token is read for each connection.
	r.p.mu.Lock()
	r.p.token = "rotated"
	r.p.mu.Unlock()
	if err := os.WriteFile(r.tokFile, []byte("rotated"), 0o600); err != nil {
		t.Fatal(err)
	}
	resp, err := r.client().Get("https://allowed.test/")
	if err != nil {
		t.Fatalf("after rotation: %v", err)
	}
	resp.Body.Close()
}

func TestNoTokenIsRejected(t *testing.T) {
	g := &Gateway{PlatformURL: "http://127.0.0.1:1"}
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, httptest.NewRequest(http.MethodConnect, "allowed.test:443", nil))
	if rec.Code != http.StatusProxyAuthRequired {
		t.Fatalf("status %d", rec.Code)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
