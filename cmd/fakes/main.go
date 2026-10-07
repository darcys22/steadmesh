// Command fakes serves the deterministic Slack, Linear and model fakes used by
// the e2e tests, so they can run as a Deployment in kind (image
// steadmesh/fakes:dev). The model fake is the scripted endpoint harness
// conformance uses (modelstub), speaking all three model APIs.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/darcys22/steadmesh/harnesses/conformance/modelstub"
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
		modelAddr  = flag.String("model-addr", ":8092", "listen address for the fake model endpoint (/v1/...)")
		modelKeys  = flag.String("model-api-keys", "", "comma-separated accepted model API keys (empty accepts any)")
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

	var keys []string
	if *modelKeys != "" {
		keys = strings.Split(*modelKeys, ",")
	}
	modelFake := modelstub.New(keys...)

	servers := []*http.Server{
		{Addr: *slackAddr, Handler: withHealth(slackFake), ReadHeaderTimeout: 10 * time.Second},
		{Addr: *linearAddr, Handler: withHealth(linearFake), ReadHeaderTimeout: 10 * time.Second},
		{Addr: *modelAddr, Handler: withHealth(modelHandler(modelFake)), ReadHeaderTimeout: 10 * time.Second},
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

// modelHandler serves the model stub with test endpoints:
//
//	GET  /_test/requests  recorded requests
//	POST /_test/keys      {"valid":[...]} replaces the accepted keys (empty: any)
//	POST /_test/reset     forgets recorded requests
func modelHandler(s *modelstub.Stub) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_test/requests", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.Requests())
	})
	mux.HandleFunc("POST /_test/keys", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Valid []string `json:"valid"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.SetKeys(body.Valid...)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("POST /_test/reset", func(w http.ResponseWriter, _ *http.Request) {
		s.Reset()
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.Handle("/", s)
	return mux
}
