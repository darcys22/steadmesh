package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/runtimeapi/client"
)

// MCPName maps a canonical dotted tool name to its MCP name.
func MCPName(canonical string) string { return strings.ReplaceAll(canonical, ".", "_") }

func stdioTransport() mcp.Transport { return &mcp.StdioTransport{} }

// buildServer lists the seat's tools and registers a proxy handler for each.
func buildServer(ctx context.Context, c *client.Client, logw io.Writer) (*mcp.Server, map[string]string, error) {
	tools, err := c.ListTools(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list tools: %w", err)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "steadmesh", Version: "v1"}, &mcp.ServerOptions{
		Instructions: "Steadmesh platform tools. Authorisation is enforced by the platform; " +
			"results are bounded JSON with stable ids and cursors for pagination.",
	})
	names := map[string]string{}
	for _, t := range tools {
		mn := MCPName(t.Name)
		if prev, dup := names[mn]; dup {
			fmt.Fprintf(logw, "steadmesh-tools: skipping %q: MCP name %q already used by %q\n", t.Name, mn, prev)
			continue
		}
		names[mn] = t.Name
		canonical := t.Name
		srv.AddTool(&mcp.Tool{
			Name:        mn,
			Description: t.Description,
			InputSchema: objectSchema(t.InputSchema),
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args json.RawMessage
			if req != nil && req.Params != nil {
				args = req.Params.Arguments
			}
			return proxyCall(ctx, c, canonical, args), nil
		})
	}
	return srv, names, nil
}

// objectSchema returns the descriptor's schema, guaranteeing a JSON object
// with type "object" as MCP requires.
func objectSchema(raw json.RawMessage) any {
	var m map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil || m == nil {
		return map[string]any{"type": "object"}
	}
	if m["type"] != "object" {
		return map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": true}
	}
	return m
}

// proxyCall forwards a tool call. Tool-level failures (is_error) and
// transport failures are both returned as MCP tool errors so the model sees
// them; neither changes authority.
func proxyCall(ctx context.Context, c *client.Client, canonical string, args json.RawMessage) *mcp.CallToolResult {
	res, err := c.CallTool(ctx, canonical, args)
	if err != nil {
		msg := err.Error()
		if client.IsFenced(err) {
			msg = "this execution has been fenced (a newer execution holds the seat lease); stop and do not retry: " + msg
		}
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
	}
	text := string(res.Content)
	if text == "" {
		text = "null"
	}
	return &mcp.CallToolResult{IsError: res.IsError, Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func serveMCP(ctx context.Context, c *client.Client, t mcp.Transport, logw io.Writer) error {
	srv, _, err := buildServer(ctx, c, logw)
	if err != nil {
		return err
	}
	return srv.Run(ctx, t)
}

var _ = runtimeapi.ToolCallResult{}
