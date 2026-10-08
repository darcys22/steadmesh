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
	"github.com/darcys22/steadmesh/connectors/browsersession"
	"github.com/darcys22/steadmesh/connectors/github"
	"github.com/darcys22/steadmesh/connectors/linear"
	"github.com/darcys22/steadmesh/connectors/model"
	"github.com/darcys22/steadmesh/connectors/secretref"
	"github.com/darcys22/steadmesh/connectors/slack"
	"github.com/darcys22/steadmesh/connectors/terminal"
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

func envDuration(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return d
	}
	return def
}

type config struct {
	databaseURL, listenAddr, controllerUser, consoleUser, secretsNamespace  string
	vaultAddr, vaultRole, vaultMount, vaultJWT                              string
	credentialRefresh, credentialGrace, credentialMaxStale, retirementGrace time.Duration
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
	flag.DurationVar(&c.credentialRefresh, "credential-refresh-interval", envDuration("CREDENTIAL_REFRESH_INTERVAL", connections.DefaultRefreshInterval),
		"how often every connection secret is re-resolved (Vault poll and watch backstop)")
	flag.DurationVar(&c.credentialGrace, "credential-grace", envDuration("CREDENTIAL_GRACE", connections.DefaultGrace),
		"how long the previous credential stays in use after its replacement is rejected or its secret is deleted")
	flag.DurationVar(&c.credentialMaxStale, "credential-max-stale", envDuration("CREDENTIAL_MAX_STALE", connections.DefaultMaxStale),
		"how long a credential stays in use while its secret cannot be read")
	flag.DurationVar(&c.retirementGrace, "retirement-grace", envDuration("RETIREMENT_GRACE", store.DefaultRetirementGrace),
		"how long a seat removed from the declaration may wind down (finish its turn, hand over) before it is retired; 0 retires it at once")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(c, log); err != nil {
		log.Error("platform stopped", "error", err)
		os.Exit(1)
	}
}

// retirementGrace maps the flag to platform.Options, where zero means the
// default and a negative value means none.
func retirementGrace(d time.Duration) time.Duration {
	if d <= 0 {
		return -1
	}
	return d
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
	var vaultLogin *secretref.Vault
	if c.vaultAddr != "" {
		v, err := secretref.NewVault(c.vaultAddr, c.vaultRole, c.vaultMount, c.vaultJWT)
		if err != nil {
			return fmt.Errorf("vault: %w", err)
		}
		resolvers["vault"] = v
		vaultLogin = v
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	if vaultLogin != nil {
		// The platform's own Vault login, separate from the credentials
		// stored in Vault (those are reported per connection).
		reg.MustRegister(
			prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "steadmesh_vault_login_ok",
				Help: "1 when the platform holds a valid Vault login."}, func() float64 {
				if vaultLogin.LoginStatus().LoggedIn {
					return 1
				}
				return 0
			}),
			prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "steadmesh_vault_login_expiry_seconds",
				Help: "Seconds until the platform's Vault login lease ends (0 when it does not expire)."}, func() float64 {
				if exp := vaultLogin.LoginStatus().ExpiresAt; !exp.IsZero() {
					return time.Until(exp).Seconds()
				}
				return 0
			}),
		)
	}
	p := platform.New(ctx, platform.Options{
		Store:   st,
		Auth:    auth.NewTokenReview(kube, st, c.controllerUser, c.consoleUser),
		Console: c.consoleUser != "",
		Factories: connections.Factories{
			Communication: map[string]func(connectors.Config) (connectors.Communication, error){"slack": slack.New, "terminal": terminal.New},
			Tracker:       map[string]func(connectors.Config) (connectors.Tracker, error){"linear": linear.New, "github": github.New, "browser_session": browsersession.New},
			Model:         map[string]func(connectors.Config) (connectors.Model, error){"anthropic": model.New, "openai": model.New, "model": model.New},
		},
		Secrets:            secretref.NewMulti(resolvers),
		CredentialRefresh:  c.credentialRefresh,
		CredentialGrace:    c.credentialGrace,
		CredentialMaxStale: c.credentialMaxStale,
		RetirementGrace:    retirementGrace(c.retirementGrace),
		Registry:           reg,
		Log:                log,
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
