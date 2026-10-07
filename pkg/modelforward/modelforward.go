// Package modelforward is the seat's local model forwarder. Harness
// processes send model requests to it on loopback; it adds the seat's
// current projected token, read for every request, the lease generation and
// the current execution, and passes them to the platform model proxy:
//
//	http://127.0.0.1:<port>/model/<connection>/v1/... -> <platform>/v1/model/<connection>/v1/...
//
// A rotated token therefore applies at once inside long-running harness
// processes, without relying on each harness's own credential refresh.
package modelforward

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

// LocalAPIKey is the placeholder credential harnesses are configured with.
// The forwarder replaces whatever credential a harness sends.
const LocalAPIKey = "steadmesh-local"

// Options configure a forwarder.
type Options struct {
	PlatformURL string
	TokenFile   string
	// Generation and Execution supply the headers for each request.
	Generation func() int64
	Execution  func() string
	// Observe, when set, receives the execution and status of every response.
	Observe func(execution string, status int)
	Log     *slog.Logger
}

// Forwarder serves the platform model proxy on loopback.
type Forwarder struct {
	ln        net.Listener
	srv       *http.Server
	tokenFile string
}

// Start listens on a loopback port and serves until Close.
func Start(o Options) (*Forwarder, error) {
	platformURL, tokenFile, gen, execution, observe, log := o.PlatformURL, o.TokenFile, o.Generation, o.Execution, o.Observe, o.Log
	if log == nil {
		log = slog.Default()
	}
	if gen == nil {
		gen = func() int64 { return 0 }
	}
	if execution == nil {
		execution = func() string { return "" }
	}
	target, err := url.Parse(platformURL)
	if err != nil || target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("model forwarder: invalid platform URL %q", platformURL)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("model forwarder: %w", err)
	}
	f := &Forwarder{ln: ln, tokenFile: tokenFile}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			conn, rest := splitModelPath(pr.In.URL.Path)
			u := *target
			u.Path = strings.TrimSuffix(target.Path, "/") + runtimeapi.PathModelProxy + url.PathEscape(conn) + rest
			u.RawPath = ""
			u.RawQuery = pr.In.URL.RawQuery
			pr.Out.URL = &u
			pr.Out.Host = target.Host
			for _, h := range []string{"Authorization", "X-Api-Key", "Proxy-Authorization", "Cookie"} {
				pr.Out.Header.Del(h)
			}
			if tok, err := f.token(); err == nil {
				pr.Out.Header.Set("Authorization", "Bearer "+tok)
			} else {
				log.Warn("model forwarder: seat token unreadable", "err", err)
			}
			pr.Out.Header.Set(runtimeapi.HeaderGeneration, strconv.FormatInt(gen(), 10))
			if id := execution(); id != "" {
				pr.Out.Header.Set(runtimeapi.HeaderExecution, id)
			} else {
				pr.Out.Header.Del(runtimeapi.HeaderExecution)
			}
		},
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			if observe != nil {
				observe(resp.Request.Header.Get(runtimeapi.HeaderExecution), resp.StatusCode)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if observe != nil {
				observe(execution(), http.StatusBadGateway)
			}
			if errors.Is(err, context.Canceled) {
				return
			}
			log.Warn("model forwarder: platform unreachable", "path", r.URL.Path, "err", err)
			http.Error(w, "platform model proxy unavailable", http.StatusBadGateway)
		},
	}
	mux := http.NewServeMux()
	// Claude Code checks connectivity with HEAD <base>/api/hello at startup.
	mux.HandleFunc("HEAD /model/{connection}/api/hello", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("/model/", func(w http.ResponseWriter, r *http.Request) {
		if conn, rest := splitModelPath(r.URL.Path); conn == "" || !strings.HasPrefix(rest, "/v1/") {
			http.Error(w, "use /model/<connection>/v1/...", http.StatusNotFound)
			return
		}
		proxy.ServeHTTP(w, r)
	})
	f.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := f.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("model forwarder stopped", "err", err)
		}
	}()
	return f, nil
}

// splitModelPath splits /model/<conn>/<rest> into conn and /<rest>.
func splitModelPath(p string) (string, string) {
	p = strings.TrimPrefix(p, "/model/")
	conn, rest, _ := strings.Cut(p, "/")
	conn, err := url.PathUnescape(conn)
	if err != nil {
		return "", ""
	}
	return conn, "/" + rest
}

func (f *Forwarder) token() (string, error) {
	b, err := os.ReadFile(f.tokenFile)
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", errors.New("token file is empty")
	}
	return tok, nil
}

// BaseURL is the base URL a harness uses for a connection (no /v1).
func (f *Forwarder) BaseURL(conn string) string {
	return "http://" + f.ln.Addr().String() + "/model/" + url.PathEscape(conn)
}

func (f *Forwarder) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = f.srv.Shutdown(ctx)
}
