// Command seat-runner is the in-Pod seat supervisor (contracts.md, Seat Pod
// contract).
//
//	seat-runner [run]     supervise the seat: lease, bootstrap, harness, inbox loop
//	seat-runner netprobe --allowed <url> --denied <url> [--timeout 3s]
//
// run acquires the seat lease (renewing it every 10s and killing the harness
// if it is fenced), loads the bootstrap and manifest, prepares the harness
// selected by STEADMESH_HARNESS (fake or claude-code), and then long-polls
// the inbox. Each delivery is executed, its events streamed, acknowledged and
// checkpointed. Probe deliveries are handled by the runner itself. On SIGTERM
// it quiesces (bounded by 240s), checkpoints, reports Stopped and releases
// the lease.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/harnesses/claudecode"
	"github.com/darcys22/steadmesh/harnesses/fake"
)

func main() {
	args := os.Args[1:]
	cmd := "run"
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "run":
		os.Exit(runMain())
	case "netprobe":
		os.Exit(netprobeMain(args, os.Stdout, os.Stderr, "/dev/termination-log"))
	case "help", "-h", "--help":
		fmt.Println("usage: seat-runner [run] | netprobe --allowed <url> --denied <url> [--timeout 3s]")
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "seat-runner: unknown command %q\n", cmd)
		os.Exit(exitError)
	}
}

// DefaultAdapters returns the built-in harness adapters.
func DefaultAdapters(name string) (harnesses.Adapter, error) {
	switch name {
	case fake.AdapterName:
		return fake.New(), nil
	case claudecode.AdapterName, "claudecode", "claude":
		return claudecode.New(), nil
	default:
		return nil, fmt.Errorf("unknown harness adapter %q (supported: %s, %s)", name, fake.AdapterName, claudecode.AdapterName)
	}
}

func runMain() int {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := ConfigFromEnv()
	if err != nil {
		log.Error("invalid configuration", "err", err)
		return exitError
	}
	log = log.With("seat", cfg.SeatKey, "seat_id", cfg.SeatID, "pod_uid", cfg.PodUID)
	r, err := NewRunner(cfg, log, DefaultAdapters)
	if err != nil {
		log.Error("runner", "err", err)
		return exitError
	}
	stop, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer cancel()

	if cfg.HealthAddr != "" {
		ln, err := net.Listen("tcp", cfg.HealthAddr)
		if err != nil {
			log.Error("health listener", "addr", cfg.HealthAddr, "err", err)
			return exitError
		}
		srv := &http.Server{Handler: r.Handler(), ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("health server", "err", err)
			}
		}()
		defer func() {
			sctx, c := context.WithTimeout(context.Background(), 2*time.Second)
			defer c()
			_ = srv.Shutdown(sctx)
		}()
	}

	err = r.Run(stop)
	code := ExitCode(err)
	if err != nil {
		log.Error("seat-runner exiting", "err", err, "exit_code", code)
	} else {
		log.Info("seat-runner exiting", "exit_code", code)
	}
	return code
}
