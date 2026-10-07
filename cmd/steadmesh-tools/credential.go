package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/runtimeapi/client"
)

// credential implements:
//
//	steadmesh-tools credential git get|store|erase   git credential helper
//	steadmesh-tools credential token [connection]     print a GitHub token (gh, scripts)
//
// Credentials come from the platform only while the seat's access profiles
// deliver them to the sandbox; every fetch is recorded on the seat's run.
func credential(ctx context.Context, c *client.Client, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: steadmesh-tools credential git <get|store|erase> | token [connection]")
		return exitUsage
	}
	switch args[0] {
	case "git":
		if len(args) < 2 || args[1] != "get" {
			return exitOK // store and erase: nothing is kept locally
		}
		attrs := map[string]string{}
		sc := bufio.NewScanner(stdin)
		for sc.Scan() {
			if k, v, ok := strings.Cut(sc.Text(), "="); ok {
				attrs[k] = v
			}
		}
		conn, err := githubConnection(ctx, c, attrs["host"], "")
		if err != nil {
			fmt.Fprintln(stderr, "steadmesh-tools credential:", err)
			return exitToolError
		}
		cred, err := c.Credential(ctx, conn)
		if err != nil {
			fmt.Fprintln(stderr, "steadmesh-tools credential:", err)
			return exitToolError
		}
		fmt.Fprintf(stdout, "username=%s\npassword=%s\n", firstNonEmpty(cred.Username, "x-access-token"), cred.Token)
		return exitOK
	case "token":
		name := ""
		if len(args) > 1 {
			name = args[1]
		}
		conn, err := githubConnection(ctx, c, "", name)
		if err != nil {
			fmt.Fprintln(stderr, "steadmesh-tools credential:", err)
			return exitToolError
		}
		cred, err := c.Credential(ctx, conn)
		if err != nil {
			fmt.Fprintln(stderr, "steadmesh-tools credential:", err)
			return exitToolError
		}
		fmt.Fprintln(stdout, cred.Token)
		return exitOK
	}
	fmt.Fprintf(stderr, "steadmesh-tools credential: unknown kind %q\n", args[0])
	return exitUsage
}

// githubConnection picks the seat's sandbox-delivered github connection for
// a host or by name.
func githubConnection(ctx context.Context, c *client.Client, host, name string) (string, error) {
	acc, err := c.Access(ctx)
	if err != nil {
		return "", err
	}
	var match []runtimeapi.GitHubSandbox
	for _, g := range acc.GitHub {
		if (name == "" || g.Connection == name) && (host == "" || strings.EqualFold(g.Host, host)) {
			match = append(match, g)
		}
	}
	switch {
	case len(match) == 1:
		return match[0].Connection, nil
	case len(match) == 0 && host != "":
		return "", fmt.Errorf("no access profile of this seat delivers credentials for %s", host)
	case len(match) == 0:
		return "", errors.New("no access profile of this seat delivers github credentials to the sandbox")
	}
	return "", errors.New("several github connections match; name one: steadmesh-tools credential token <connection>")
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
