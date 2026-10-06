// Command fakes serves the deterministic Slack and Linear fakes used by the
// e2e tests, so they can run as a Deployment in kind (image steadmesh/fakes:dev).
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	fakelinear "github.com/darcys22/steadmesh/tests/fakes/linear"
	fakeslack "github.com/darcys22/steadmesh/tests/fakes/slack"
)

func main() {
	var (
		slackAddr  = flag.String("slack-addr", ":8090", "listen address for the fake Slack API and Socket Mode")
		linearAddr = flag.String("linear-addr", ":8091", "listen address for the fake Linear GraphQL API")
		teamID     = flag.String("slack-team", "T0FAKE", "fake Slack team id")
		users      = flag.String("slack-users", "U0ALICE=alice,U0BOB=bob", "comma-separated id=name Slack users")
		botToken   = flag.String("slack-bot-token", "", "required bot token (empty accepts any xoxb- token)")
		appToken   = flag.String("slack-app-token", "", "required app token (empty accepts any xapp- token)")
		redeliver  = flag.Duration("slack-redelivery-timeout", 3*time.Second, "redeliver unacked envelopes after this long")
		maxRetries = flag.Int("slack-max-retries", 3, "redeliveries per envelope (negative disables)")
		linearKey  = flag.String("linear-api-key", "", "required Linear API key (empty accepts any)")
		linearOrg  = flag.String("linear-org", "org-fake", "fake Linear organisation id")
		teams      = flag.String("linear-teams", "team-eng=ENG", "comma-separated id=key Linear teams")
	)
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	userMap, err := pairs(*users)
	if err != nil {
		log.Error("bad --slack-users", "error", err)
		os.Exit(2)
	}
	teamMap, err := pairs(*teams)
	if err != nil {
		log.Error("bad --linear-teams", "error", err)
		os.Exit(2)
	}

	slackFake := fakeslack.New(fakeslack.Options{
		TeamID: *teamID, Users: userMap, BotToken: *botToken, AppToken: *appToken,
		RedeliveryTimeout: *redeliver, MaxRetries: *maxRetries,
	})
	defer slackFake.Close()
	linearFake := fakelinear.New(fakelinear.Options{APIKey: *linearKey, OrganizationID: *linearOrg, Teams: teamMap})

	servers := []*http.Server{
		{Addr: *slackAddr, Handler: withHealth(slackFake), ReadHeaderTimeout: 10 * time.Second},
		{Addr: *linearAddr, Handler: withHealth(linearFake), ReadHeaderTimeout: 10 * time.Second},
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, len(servers))
	for _, s := range servers {
		log.Info("listening", "addr", s.Addr)
		go func() {
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}
	select {
	case <-ctx.Done():
	case err := <-errc:
		log.Error("server failed", "error", err)
		os.Exit(1)
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutdown)
	}
}

func withHealth(h http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.Handle("/", h)
	return mux
}

func pairs(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" || v == "" {
			return nil, errors.New("expected id=value, got " + kv)
		}
		out[k] = v
	}
	return out, nil
}
