// Command console runs the Steadmesh Console, a read-only browser view of
// the organisation: seats, runs, handoffs, outputs and readiness. It reads the
// platform's /console/v1 API and the cluster's Steadmesh resources and seat
// Pods. It is deployed only when the platform stage sets enable_console.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/tools/clientcmd"

	"github.com/darcys22/steadmesh/console"
)

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

const defaultTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"

func main() {
	listen := flag.String("listen-addr", env("LISTEN_ADDR", "127.0.0.1:8090"), "address to serve the console on")
	platformURL := flag.String("platform-url", env("PLATFORM_URL", "http://steadmesh-platform.steadmesh-system.svc:8080"), "platform service URL")
	tokenFile := flag.String("token-file", env("TOKEN_FILE", defaultTokenFile), "ServiceAccount token presented to the platform")
	kubeconfig := flag.String("kubeconfig", env("KUBECONFIG", ""), "kubeconfig path (default: in-cluster or standard loading rules)")
	kctx := flag.String("context", env("KUBE_CONTEXT", ""), "kubeconfig context (out-of-cluster only)")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(*listen, *platformURL, *tokenFile, *kubeconfig, *kctx, log); err != nil {
		log.Error("console stopped", "error", err)
		os.Exit(1)
	}
}

func run(listen, platformURL, tokenFile, kubeconfig, kctx string, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = kubeconfig
	kcfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: kctx}).ClientConfig()
	if err != nil {
		return fmt.Errorf("kubernetes config: %w", err)
	}
	cluster, err := console.NewKubeCluster(ctx, kcfg, time.Minute)
	if err != nil {
		return fmt.Errorf("cluster: %w", err)
	}
	h, err := console.New(console.Config{
		Platform: console.NewPlatformClient(platformURL, tokenFile),
		Cluster:  cluster,
		Log:      log,
	})
	if err != nil {
		return err
	}
	// No WriteTimeout: the activity stream is long-lived.
	srv := &http.Server{Addr: listen, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("console listening", "addr", listen, "platform", platformURL)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	return srv.Shutdown(shutdown)
}
