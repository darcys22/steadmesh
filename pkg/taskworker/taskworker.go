// Package taskworker defines the narrowed bootstrap for disposable task runs.
package taskworker

import (
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"slices"
	"strings"
)

type Bootstrap struct {
	Run      runtimeapi.TaskRun   `json:"run"`
	Manifest compile.SeatManifest `json:"manifest"`
	Identity runtimeapi.Bootstrap `json:"identity"`
}

// Manifest intersects current parent grants with the worker's fixed read-only
// surface. It never adds a grant, route, access profile or workspace binding.
func Manifest(parent compile.SeatManifest, toolNames []string) compile.SeatManifest {
	out := parent
	out.Capabilities = nil
	out.Access = nil
	out.SendTo = nil
	out.ReceiveFrom = nil
	out.ChannelBindings = nil
	out.IsRepresentative = false
	out.PersonalMemory = ""
	out.AdoptFrom = ""
	out.Workspace.Persistent = false
	out.Workspace.Shared = nil
	for _, c := range parent.Capabilities {
		allowed := []string{}
		if strings.HasPrefix(c.Resource, "memory:") && len(toolNames) > 0 {
			for _, op := range c.Operations {
				if slices.Contains([]string{"read", "list", "search", "history"}, op) {
					allowed = append(allowed, op)
				}
			}
		}
		if parent.Harness.Model != nil && c.Resource == "connection:"+parent.Harness.Model.Connection && slices.Contains(c.Operations, "model.infer") {
			allowed = []string{"model.infer"}
		}
		if len(allowed) > 0 {
			c.Operations = allowed
			out.Capabilities = append(out.Capabilities, c)
		}
	}
	return out
}
