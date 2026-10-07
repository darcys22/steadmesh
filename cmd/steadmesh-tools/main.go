// Command steadmesh-tools is the seat's tool client. It exposes the platform
// tools permitted for the seat over three transports with identical
// authority (authorisation is always enforced server-side):
//
//	steadmesh-tools mcp                 stdio MCP server (tool names use _ for .)
//	steadmesh-tools call <name> [json]  invoke one tool; json may be "-" for stdin
//	steadmesh-tools list                print tool descriptors as JSON
//	steadmesh-tools credential ...      git credential helper; GitHub token for gh
//	steadmesh-tools browser-mcp         the browser plugin's MCP server (Playwright MCP)
//
// Configuration comes from the seat environment: STEADMESH_PLATFORM_URL,
// STEADMESH_TOKEN_FILE (re-read per request), and the lease generation and
// execution id from STEADMESH_GENERATION / STEADMESH_EXECUTION or the files the
// runner writes (/seat/runner/generation, /seat/runner/execution).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/darcys22/steadmesh/pkg/runtimeapi/client"
)

// Exit codes for call.
const (
	exitOK        = 0
	exitToolError = 1
	exitUsage     = 2
	exitTransport = 3
	exitFenced    = 4
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: steadmesh-tools mcp | call <name> [json|-] | list | credential git <op> | credential token [connection]")
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	c, err := client.FromEnv("steadmesh-tools")
	if err != nil {
		fmt.Fprintln(stderr, "steadmesh-tools:", err)
		return exitUsage
	}
	switch args[0] {
	case "mcp":
		if err := serveMCP(ctx, c, stdioTransport(), stderr); err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintln(stderr, "steadmesh-tools mcp:", err)
			return exitTransport
		}
		return exitOK
	case "call":
		if len(args) < 2 || len(args) > 3 {
			usage(stderr)
			return exitUsage
		}
		var raw []byte
		switch {
		case len(args) == 2:
			raw = []byte("{}")
		case args[2] == "-":
			raw, err = io.ReadAll(io.LimitReader(stdin, 4<<20))
			if err != nil {
				fmt.Fprintln(stderr, "steadmesh-tools: read stdin:", err)
				return exitUsage
			}
		default:
			raw = []byte(args[2])
		}
		return callTool(ctx, c, args[1], raw, stdout, stderr)
	case "browser-mcp":
		return browserMCP(ctx, c, stdin, stdout, stderr)
	case "credential":
		return credential(ctx, c, args[1:], stdin, stdout, stderr)
	case "list":
		tools, err := c.ListTools(ctx)
		if err != nil {
			fmt.Fprintln(stderr, "steadmesh-tools:", err)
			return transportExit(err)
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(tools)
		return exitOK
	case "-h", "--help", "help":
		usage(stdout)
		return exitOK
	default:
		usage(stderr)
		return exitUsage
	}
}

func callTool(ctx context.Context, c *client.Client, name string, raw []byte, stdout, stderr io.Writer) int {
	if !json.Valid(raw) {
		fmt.Fprintln(stderr, "steadmesh-tools: arguments are not valid JSON")
		return exitUsage
	}
	res, err := c.CallTool(ctx, name, raw)
	if err != nil {
		fmt.Fprintln(stderr, "steadmesh-tools:", err)
		return transportExit(err)
	}
	content := res.Content
	if len(content) == 0 {
		content = json.RawMessage("null")
	}
	fmt.Fprintln(stdout, string(content))
	if res.IsError {
		return exitToolError
	}
	return exitOK
}

func transportExit(err error) int {
	if client.IsFenced(err) {
		return exitFenced
	}
	return exitTransport
}
