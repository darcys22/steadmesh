// Command egress runs the egress gateway (services/egress): the HTTP proxy
// that seats with egress access reach through their seat runner. It needs
// the platform URL and nothing else: no credentials, no Kubernetes access.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/darcys22/steadmesh/services/egress"
)

func main() {
	listen := flag.String("listen", ":3128", "proxy listen address")
	platform := flag.String("platform-url", "", "platform base URL, e.g. http://steadmesh-platform.steadmesh-system.svc:8080")
	recheck := flag.Duration("recheck", 3*time.Second, "how often open tunnels are re-authorised")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if *platform == "" {
		log.Error("--platform-url is required")
		os.Exit(2)
	}
	g := &egress.Gateway{PlatformURL: *platform, Recheck: *recheck, Log: log}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go g.Run(ctx)
	srv := &http.Server{Addr: *listen, Handler: g, ReadHeaderTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("egress gateway listening", "addr", *listen, "platform", *platform)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("egress gateway", "err", err)
		os.Exit(1)
	}
}
