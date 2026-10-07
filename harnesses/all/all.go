// Package all registers every built-in harness adapter. Import it for its
// side effect wherever adapters are looked up by name.
package all

import (
	_ "github.com/darcys22/steadmesh/harnesses/claudecode"
	_ "github.com/darcys22/steadmesh/harnesses/codex"
	_ "github.com/darcys22/steadmesh/harnesses/fake"
	_ "github.com/darcys22/steadmesh/harnesses/pi"
)
