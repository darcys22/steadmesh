package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/darcys22/steadmesh/pkg/runtimeapi/client"
)

// Config is the runner configuration, read from the Seat Pod environment
// (docs/architecture.html). Durations can be overridden for tests
// and tuning with STEADMESH_RUNNER_* variables.
type Config struct {
	PlatformURL     string
	TokenFile       string
	OrgID           string
	SeatID          string
	SeatKey         string
	ConfigRevision  string
	Harness         string
	ModelConnection string
	Model           string
	ManifestDir     string
	PodUID          string

	WorkspaceDir string
	HomeDir      string
	RunnerDir    string
	TmpDir       string
	ToolCommand  string
	HealthAddr   string

	LeaseAcquireTimeout time.Duration
	LeaseRetryInterval  time.Duration
	RenewInterval       time.Duration
	LeaseTTL            time.Duration
	PollWait            time.Duration
	TurnTimeout         time.Duration
	QuiesceTimeout      time.Duration
	BootstrapTimeout    time.Duration
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func durEnv(k string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(k)
	if v == "" {
		return def, nil
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", k, err)
	}
	return d, nil
}

// ConfigFromEnv reads the configuration.
func ConfigFromEnv() (Config, error) {
	c := Config{
		PlatformURL:     os.Getenv(client.EnvPlatformURL),
		TokenFile:       getenv(client.EnvTokenFile, client.DefaultTokenFile),
		OrgID:           os.Getenv("STEADMESH_ORG_ID"),
		SeatID:          os.Getenv("STEADMESH_SEAT_ID"),
		SeatKey:         os.Getenv("STEADMESH_SEAT_KEY"),
		ConfigRevision:  os.Getenv("STEADMESH_CONFIG_REVISION"),
		Harness:         getenv("STEADMESH_HARNESS", "fake"),
		ModelConnection: os.Getenv("STEADMESH_MODEL_CONNECTION"),
		Model:           os.Getenv("STEADMESH_MODEL"),
		ManifestDir:     getenv("STEADMESH_MANIFEST_DIR", "/etc/steadmesh/manifest"),
		PodUID:          os.Getenv("POD_UID"),
		WorkspaceDir:    getenv("STEADMESH_WORKSPACE_DIR", "/seat/workspace"),
		HomeDir:         getenv("STEADMESH_HOME_DIR", "/seat/home"),
		RunnerDir:       client.RunnerDir(),
		TmpDir:          getenv("TMPDIR", "/tmp"),
		ToolCommand:     getenv("STEADMESH_TOOLS_BIN", "/usr/local/bin/steadmesh-tools"),
		HealthAddr:      getenv("STEADMESH_HEALTH_ADDR", ":8081"),
	}
	if c.PlatformURL == "" {
		return c, fmt.Errorf("%s is required", client.EnvPlatformURL)
	}
	var err error
	for _, d := range []struct {
		dst *time.Duration
		key string
		def time.Duration
	}{
		{&c.LeaseAcquireTimeout, "STEADMESH_RUNNER_LEASE_ACQUIRE_TIMEOUT", 3 * time.Minute},
		{&c.LeaseRetryInterval, "STEADMESH_RUNNER_LEASE_RETRY_INTERVAL", 2 * time.Second},
		{&c.RenewInterval, "STEADMESH_RUNNER_RENEW_INTERVAL", 10 * time.Second},
		{&c.LeaseTTL, "STEADMESH_RUNNER_LEASE_TTL", 30 * time.Second},
		{&c.PollWait, "STEADMESH_RUNNER_POLL_WAIT", 25 * time.Second},
		{&c.TurnTimeout, "STEADMESH_RUNNER_TURN_TIMEOUT", 20 * time.Minute},
		{&c.QuiesceTimeout, "STEADMESH_RUNNER_QUIESCE_TIMEOUT", 240 * time.Second},
		{&c.BootstrapTimeout, "STEADMESH_RUNNER_BOOTSTRAP_TIMEOUT", 2 * time.Minute},
	} {
		if *d.dst, err = durEnv(d.key, d.def); err != nil {
			return c, err
		}
	}
	return c, nil
}
