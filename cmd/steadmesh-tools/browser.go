package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/darcys22/steadmesh/pkg/runtimeapi/client"
)

// browserCheckInterval is how often browser-mcp re-checks the seat's access.
var browserCheckInterval = 3 * time.Second

// browserMCP is the browser plugin's MCP server (steadmesh-tools
// browser-mcp). It runs Playwright MCP over stdio with a headless, isolated
// Chromium whose traffic goes through the seat's egress proxy, loads the
// granted signed-in session, and re-checks the seat's access every few
// seconds: when the browser is no longer granted it kills the browser and
// exits, so a call in progress fails.
func browserMCP(ctx context.Context, c *client.Client, stdin io.Reader, stdout, stderr io.Writer) int {
	acc, err := c.Access(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "steadmesh-tools browser-mcp:", err)
		return exitTransport
	}
	if !acc.Browser {
		fmt.Fprintln(stderr, "steadmesh-tools browser-mcp: no access profile of this seat grants a browser")
		return exitToolError
	}
	exe := os.Getenv("STEADMESH_BROWSER_EXECUTABLE")
	if exe == "" {
		exe = "/usr/bin/chromium"
	}
	// Chromium's own sandbox needs privileges a restricted seat Pod lacks;
	// the Pod is the boundary.
	args := []string{"--headless", "--isolated", "--no-sandbox", "--executable-path", exe, "--output-dir", filepath.Join(os.TempDir(), "steadmesh-browser-output")}
	if proxy := os.Getenv("HTTPS_PROXY"); proxy != "" {
		args = append(args, "--proxy-server", proxy)
	}
	dir, err := os.MkdirTemp("", "steadmesh-browser-")
	if err != nil {
		fmt.Fprintln(stderr, "steadmesh-tools browser-mcp:", err)
		return exitToolError
	}
	defer os.RemoveAll(dir)
	if acc.BrowserSession != "" {
		cred, err := c.Credential(ctx, acc.BrowserSession)
		if err != nil {
			fmt.Fprintln(stderr, "steadmesh-tools browser-mcp: browser session:", err)
			return exitToolError
		}
		state := filepath.Join(dir, "storage-state.json")
		if err := os.WriteFile(state, cred.Data, 0o600); err != nil {
			fmt.Fprintln(stderr, "steadmesh-tools browser-mcp:", err)
			return exitToolError
		}
		args = append(args, "--storage-state", state)
	}
	cmd := exec.Command("playwright-mcp", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.Env = append(os.Environ(), "TMPDIR="+dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(stderr, "steadmesh-tools browser-mcp: start playwright-mcp:", err)
		return exitToolError
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	kill := func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	t := time.NewTicker(browserCheckInterval)
	defer t.Stop()
	for {
		select {
		case err := <-done:
			if err != nil {
				return exitToolError
			}
			return exitOK
		case <-ctx.Done():
			kill()
			<-done
			return exitOK
		case <-t.C:
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			acc, err := c.Access(cctx)
			cancel()
			if err == nil && acc.Browser {
				continue
			}
			if err != nil && !errors.Is(err, client.ErrUnauthenticated) && !errors.Is(err, client.ErrForbidden) {
				continue // platform unreachable: keep running
			}
			fmt.Fprintln(stderr, "steadmesh-tools browser-mcp: browser access was revoked; closing the browser")
			kill()
			<-done
			return exitToolError
		}
	}
}
