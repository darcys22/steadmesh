package harnesses

import (
	"fmt"
	"slices"
	"sort"
	"sync"
)

// Model API protocols. A harness speaks some of them; a model connection
// serves some of them; a harness profile uses one both sides support.
const (
	APIAnthropicMessages = "anthropic_messages" // POST /v1/messages
	APIOpenAIResponses   = "openai_responses"   // POST /v1/responses
	APIOpenAIChat        = "openai_chat"        // POST /v1/chat/completions
)

// ModelAPIs lists every model API protocol.
var ModelAPIs = []string{APIAnthropicMessages, APIOpenAIResponses, APIOpenAIChat}

// Setting describes one model.settings key a harness accepts.
type Setting struct {
	Description string
	// Values are the allowed values; empty means any non-empty string.
	Values []string
}

// Descriptor describes a harness adapter. Compile validation reads
// descriptors; the seat runner constructs adapters with New. Adding a harness
// means a new package that registers its descriptor, an image and a contract
// (docs/harnesses.html), with no change to the core.
type Descriptor struct {
	// Name is the harness profile adapter value.
	Name string
	// Aliases are accepted alternative names.
	Aliases []string
	// APIs are the model APIs the harness can speak, in order of preference.
	APIs []string
	// NeedsModel means a harness profile must select a model.
	NeedsModel bool
	// Settings are the model.settings keys the harness understands.
	Settings map[string]Setting
	// Capabilities are values a profile's required_capabilities may name.
	Capabilities []string
	// New returns an unprepared adapter.
	New func() Adapter
}

var (
	regMu    sync.RWMutex
	registry = map[string]Descriptor{}
	aliases  = map[string]string{}
)

// Register adds a harness. It panics on a duplicate name, as a programming error.
func Register(d Descriptor) {
	regMu.Lock()
	defer regMu.Unlock()
	if d.Name == "" || d.New == nil {
		panic("harnesses: Register needs a name and a constructor")
	}
	for _, n := range append([]string{d.Name}, d.Aliases...) {
		if _, dup := registry[n]; dup {
			panic(fmt.Sprintf("harnesses: %q registered twice", n))
		}
		if _, dup := aliases[n]; dup {
			panic(fmt.Sprintf("harnesses: %q registered twice", n))
		}
	}
	for _, api := range d.APIs {
		if !slices.Contains(ModelAPIs, api) {
			panic(fmt.Sprintf("harnesses: %s declares unknown model API %q", d.Name, api))
		}
	}
	registry[d.Name] = d
	for _, a := range d.Aliases {
		aliases[a] = d.Name
	}
}

// Lookup returns the descriptor registered under name or one of its aliases.
func Lookup(name string) (Descriptor, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	if n, ok := aliases[name]; ok {
		name = n
	}
	d, ok := registry[name]
	return d, ok
}

// Registered returns every registered descriptor, sorted by name.
func Registered() []Descriptor {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]Descriptor, 0, len(registry))
	for _, d := range registry {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
