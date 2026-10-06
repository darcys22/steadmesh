// Command platform runs the trusted platform service: the seat and controller
// runtime API, channel ingress, outbox delivery, the capability gateway and the
// wake scheduler.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/connectors/anthropic"
	"github.com/darcys22/steadmesh/connectors/linear"
	"github.com/darcys22/steadmesh/connectors/secretref"
	"github.com/darcys22/steadmesh/connectors/slack"
	"github.com/darcys22/steadmesh/services/auth"
	"github.com/darcys22/steadmesh/services/connections"
	"github.com/darcys22/steadmesh/services/platform"
	"github.com/darcys22/steadmesh/services/store"
)

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

type config struct {
	databaseURL, listenAddr, controllerUser, consoleUser, secretsNamespace string
	vaultAddr, vaultRole, vaultMount, vaultJWT                             string
}

func main() {
	var c config
	flag.StringVar(&c.databaseURL, "database-url", env("DATABASE_URL", ""), "Postgres connection string")
	flag.StringVar(&c.listenAddr, "listen-addr", env("LISTEN_ADDR", ":8080"), "address for the API, /healthz, /readyz and /metrics")
	flag.StringVar(&c.controllerUser, "controller-username", env("CONTROLLER_USERNAME", "system:serviceaccount:steadmesh-system:steadmesh-controller"),
		"the only identity accepted on /internal/v1")
	flag.StringVar(&c.consoleUser, "console-username", env("CONSOLE_USERNAME", ""),
		"the only identity accepted on the read-only /console/v1 API; empty disables the console API")
	flag.StringVar(&c.secretsNamespace, "secrets-namespace", env("SECRETS_NAMESPACE", "steadmesh-system"), "namespace of k8s: secret references")
	flag.StringVar(&c.vaultAddr, "vault-addr", env("VAULT_ADDR", ""), "Vault address; empty disables vault: secret references")
	flag.StringVar(&c.vaultRole, "vault-role", env("VAULT_ROLE", "steadmesh-platform"), "Vault Kubernetes auth role")
	flag.StringVar(&c.vaultMount, "vault-mount", env("VAULT_MOUNT", ""), "Vault KV v2 mount (default secret)")
	flag.StringVar(&c.vaultJWT, "vault-jwt-path", env("VAULT_JWT_PATH", ""), "ServiceAccount token for Vault login (default in-cluster token)")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(c, log); err != nil {
		log.Error("platform stopped", "error", err)
		os.Exit(1)
	}
}

func run(c config, log *slog.Logger) error {
	if c.databaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, c.databaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	kcfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), nil).ClientConfig()
	if err != nil {
		return fmt.Errorf("kubernetes config: %w", err)
	}
	kube, err := kubernetes.NewForConfig(kcfg)
	if err != nil {
		return err
	}
	resolvers := map[string]connectors.Secrets{"k8s": secretref.NewKubernetes(kube, c.secretsNamespace)}
	if c.vaultAddr != "" {
		v, err := secretref.NewVault(c.vaultAddr, c.vaultRole, c.vaultMount, c.vaultJWT)
		if err != nil {
			return fmt.Errorf("vault: %w", err)
		}
		resolvers["vault"] = v
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	p := platform.New(ctx, platform.Options{
		Store:   st,
		Auth:    auth.NewTokenReview(kube, st, c.controllerUser, c.consoleUser),
		Console: c.consoleUser != "",
		Factories: connections.Factories{
			Communication: map[string]func(connectors.Config) (connectors.Communication, error){"slack": slack.New},
			Tracker:       map[string]func(connectors.Config) (connectors.Tracker, error){"linear": linear.New},
			Model:         map[string]func(connectors.Config) (connectors.Model, error){"anthropic": anthropic.New},
		},
		Secrets:  secretref.NewMulti(resolvers),
		Registry: reg,
		Log:      log,
	})
	if err := p.Start(ctx); err != nil {
		return err
	}
	srv := &http.Server{Addr: c.listenAddr, Handler: p.Handler, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("platform listening", "addr", c.listenAddr)

	select {
	case err := <-errc:
		stop()
		p.Wait()
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = srv.Shutdown(shutdown)
	p.Wait()
	return err
}
