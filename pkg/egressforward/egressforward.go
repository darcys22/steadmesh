// Package egressforward is the seat's local egress proxy. Harness processes
// use it through HTTPS_PROXY/HTTP_PROXY; it passes each CONNECT tunnel or
// plain HTTP request to the egress gateway with the seat's current token
// (read from the token file for every connection), the lease generation and
// the current execution. The gateway decides what is allowed.
package egressforward

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

// Options configure a forwarder.
type Options struct {
	// GatewayURL is the egress gateway, e.g. http://steadmesh-egress.steadmesh-system.svc:3128.
	GatewayURL string
	TokenFile  string
	Generation func() int64
	Execution  func() string
	Log        *slog.Logger
}

// Forwarder is the local proxy.
type Forwarder struct {
	o       Options
	gateway string // host:port
	ln      net.Listener
	wg      sync.WaitGroup
}

// Start listens on a loopback port.
func Start(o Options) (*Forwarder, error) {
	u, err := url.Parse(o.GatewayURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("egress forwarder: invalid gateway URL %q", o.GatewayURL)
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Generation == nil {
		o.Generation = func() int64 { return 0 }
	}
	if o.Execution == nil {
		o.Execution = func() string { return "" }
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("egress forwarder: %w", err)
	}
	f := &Forwarder{o: o, gateway: u.Host, ln: ln}
	go f.serve()
	return f, nil
}

// URL is the proxy URL for HTTPS_PROXY and HTTP_PROXY.
func (f *Forwarder) URL() string { return "http://" + f.ln.Addr().String() }

// Env is the proxy environment for harness processes. Loopback and the
// platform host are reached directly.
func (f *Forwarder) Env(direct ...string) []string {
	noProxy := strings.Join(append([]string{"127.0.0.1", "localhost", "::1"}, direct...), ",")
	return []string{
		"HTTPS_PROXY=" + f.URL(), "https_proxy=" + f.URL(),
		"HTTP_PROXY=" + f.URL(), "http_proxy=" + f.URL(),
		"NO_PROXY=" + noProxy, "no_proxy=" + noProxy,
	}
}

// Close stops accepting connections.
func (f *Forwarder) Close() {
	_ = f.ln.Close()
}

func (f *Forwarder) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(c)
	}
}

func (f *Forwarder) token() string {
	b, err := os.ReadFile(f.o.TokenFile)
	if err != nil {
		f.o.Log.Warn("egress forwarder: seat token unreadable", "err", err)
		return ""
	}
	return strings.TrimSpace(string(b))
}

// handle serves one client connection: one CONNECT tunnel, or one plain
// HTTP request (the connection is closed after the response).
func (f *Forwarder) handle(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	up, err := net.DialTimeout("tcp", f.gateway, 10*time.Second)
	if err != nil {
		writeStatus(c, http.StatusBadGateway, "steadmesh: egress gateway unreachable: "+err.Error())
		return
	}
	defer up.Close()
	req.Header.Del("Proxy-Authorization")
	req.Header.Set("Proxy-Authorization", "Bearer "+f.token())
	req.Header.Set(runtimeapi.HeaderGeneration, strconv.FormatInt(f.o.Generation(), 10))
	if id := f.o.Execution(); id != "" {
		req.Header.Set(runtimeapi.HeaderExecution, id)
	}
	if req.Method != http.MethodConnect {
		req.Close = true
		req.Header.Set("Connection", "close")
		if err := req.WriteProxy(up); err != nil {
			return
		}
		_, _ = io.Copy(c, up)
		return
	}
	if err := req.Write(up); err != nil {
		return
	}
	ubr := bufio.NewReader(up)
	resp, err := http.ReadResponse(ubr, req)
	if err != nil {
		writeStatus(c, http.StatusBadGateway, "steadmesh: egress gateway: "+err.Error())
		return
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Write(c)
		return
	}
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, br); closeWrite(up); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, ubr); closeWrite(c); done <- struct{}{} }()
	<-done
	<-done
}

// closeWrite half-closes a TCP connection so the peer sees EOF.
func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

func writeStatus(c net.Conn, code int, msg string) {
	resp := &http.Response{StatusCode: code, ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{"Content-Type": {"text/plain"}},
		Body: io.NopCloser(strings.NewReader(msg + "\n")), ContentLength: int64(len(msg) + 1), Close: true}
	_ = resp.Write(c)
}
