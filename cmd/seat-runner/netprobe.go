package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// netprobeMain verifies NetworkPolicy enforcement for the controller's probe
// Pod: it exits 0 iff an HTTP GET to --allowed connects (any status) and a GET
// to --denied fails to connect. The verdict is also written to termPath
// (/dev/termination-log) so the controller can read it from Pod status.
func netprobeMain(args []string, stdout, stderr io.Writer, termPath string) int {
	fs := flag.NewFlagSet("netprobe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	allowed := fs.String("allowed", "", "URL that must be reachable")
	denied := fs.String("denied", "", "URL that must not be reachable")
	timeout := fs.Duration("timeout", 3*time.Second, "per-request timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *allowed == "" || *denied == "" {
		fmt.Fprintln(stderr, "netprobe: --allowed and --denied are required")
		return 2
	}
	allowedErr := connects(*allowed, *timeout)
	deniedErr := connects(*denied, *timeout)
	ok := allowedErr == nil && deniedErr != nil
	msg := fmt.Sprintf("netprobe: allowed %s: %s; denied %s: %s; enforced=%v", *allowed, verdict(allowedErr), *denied, verdict(deniedErr), ok)
	fmt.Fprintln(stdout, msg)
	if termPath != "" {
		_ = os.WriteFile(termPath, []byte(msg+"\n"), 0o644)
	}
	if ok {
		return 0
	}
	return 1
}

func verdict(err error) string {
	if err == nil {
		return "connected"
	}
	return "failed (" + err.Error() + ")"
}

// connects performs a GET and reports nil when a connection was made and an
// HTTP response (any status) was received.
func connects(url string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	c := &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("timeout after %v", timeout)
		}
		return err
	}
	resp.Body.Close()
	return nil
}
