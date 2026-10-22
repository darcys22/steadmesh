// Package skills reads individual skills from pinned Claude marketplaces. It
// deliberately does not install plugins, execute their code, or follow plugin
// sources outside the configured repository.
package skills

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/darcys22/steadmesh/pkg/spec"
)

const (
	// MaxBundleBytes and MaxBundleFiles bound an individual selected skill.
	MaxBundleBytes   = 8 << 20
	MaxBundleFiles   = 512
	maxArchiveBytes  = 16 << 20
	maxExpandedBytes = 64 << 20
	maxArchiveFiles  = 10000
	maxFileBytes     = 8 << 20
	maxCacheBytes    = 128 << 20
	maxCacheEntries  = 4
)

var (
	marketplaceKeyRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	identifierRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,127}$`)
	repositoryRE     = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,37}[A-Za-z0-9])?/[A-Za-z0-9_.-]{1,100}$`)
	commitRE         = regexp.MustCompile(`^[a-f0-9]{40}$`)
)

// Entry describes an individually selectable skill. Unsupported entries are
// diagnostics for plugins whose sources would require another repository,
// package manager, or executable. Such entries have no Ref.Skill and cannot be
// resolved. Consumers must not offer them as selectable skills.
type Entry struct {
	Ref         spec.SkillRef `json:"ref"`
	Description string        `json:"description,omitempty"`
	Unsupported string        `json:"unsupported,omitempty"`
}

// Bundle contains only one skill directory, with safe relative file paths.
// Supporting files are preserved as data, never executed. Digest is a hex
// SHA-256 of sorted length-prefixed file paths and contents.
type Bundle struct {
	Ref    spec.SkillRef     `json:"ref"`
	Files  map[string][]byte `json:"files"`
	Digest string            `json:"digest"`
}

// Catalog is a concurrency-safe, bounded in-memory cache of immutable
// repositories. Construct it with NewCatalog or NewCatalogWithTransport.
type Catalog struct {
	client     *http.Client
	mu         sync.Mutex
	cache      map[string]*cachedRepository
	loading    map[string]*repositoryLoad
	cacheBytes int
	tick       uint64
	fetchSlots chan struct{}
}

type cachedRepository struct {
	files map[string][]byte
	size  int
	used  uint64
}

type repositoryLoad struct {
	done chan struct{}
	repo *cachedRepository
	err  error
}

// NewCatalog uses the standard HTTP transport. No credentials or Git commands
// are used. Only public, explicitly pinned GitHub repositories are supported.
func NewCatalog() *Catalog { return NewCatalogWithTransport(http.DefaultTransport) }

// NewCatalogWithTransport enables deterministic tests and custom networking.
// URL validation, redirect restrictions, timeouts and byte limits still apply.
// Callers must not mutate the transport while the catalog is in use.
func NewCatalogWithTransport(transport http.RoundTripper) *Catalog {
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &Catalog{
		client: &http.Client{Transport: transport, Timeout: 45 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return fmt.Errorf("marketplace archive redirects are not allowed")
			}},
		cache:      make(map[string]*cachedRepository),
		loading:    make(map[string]*repositoryLoad),
		fetchSlots: make(chan struct{}, 2),
	}
}

// List returns sorted skills and unsupported-source diagnostics. Malformed
// local declarations, duplicate identifiers, and unsafe archives fail closed.
func (c *Catalog) List(ctx context.Context, key string, source spec.SkillMarketplace) ([]Entry, error) {
	index, err := c.index(ctx, key, source)
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(index.skills)+len(index.unsupported))
	for _, skill := range index.skills {
		entries = append(entries, skill.entry)
	}
	for plugin, reason := range index.unsupported {
		entries = append(entries, Entry{Ref: spec.SkillRef{Marketplace: key, Plugin: plugin}, Unsupported: reason})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Ref.Plugin != entries[j].Ref.Plugin {
			return entries[i].Ref.Plugin < entries[j].Ref.Plugin
		}
		return entries[i].Ref.Skill < entries[j].Ref.Skill
	})
	return entries, nil
}

// Resolve selects exactly one skill, not the plugin which contains it. The
// marketplace reference must match the configured key. The key is an operator
// alias; the pinned repository and commit, not its manifest name, define trust.
func (c *Catalog) Resolve(ctx context.Context, key string, source spec.SkillMarketplace, ref spec.SkillRef) (Bundle, error) {
	if ref.Marketplace != key {
		return Bundle{}, fmt.Errorf("skill marketplace %q does not match configured marketplace %q", ref.Marketplace, key)
	}
	if !identifierRE.MatchString(ref.Plugin) || !identifierRE.MatchString(ref.Skill) {
		return Bundle{}, fmt.Errorf("invalid plugin or skill identifier")
	}
	index, err := c.index(ctx, key, source)
	if err != nil {
		return Bundle{}, err
	}
	if reason, ok := index.unsupported[ref.Plugin]; ok {
		return Bundle{}, fmt.Errorf("plugin %q is unsupported: %s", ref.Plugin, reason)
	}
	skill, ok := index.skills[ref.Plugin+"/"+ref.Skill]
	if !ok {
		return Bundle{}, fmt.Errorf("skill %s/%s/%s not found", key, ref.Plugin, ref.Skill)
	}
	files := make(map[string][]byte)
	total := 0
	prefix := skill.directory + "/"
	if skill.directory == "." {
		prefix = ""
	}
	for name, content := range index.files {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		relative := strings.TrimPrefix(name, prefix)
		if excludedSkillFile(relative, skill.directory == skill.pluginRoot) {
			continue
		}
		if relative != "SKILL.md" && strings.EqualFold(pathBase(relative), "SKILL.md") {
			return Bundle{}, fmt.Errorf("skill %q contains nested SKILL.md at %q", ref.Skill, relative)
		}
		total += len(content)
		if total > MaxBundleBytes || len(files) >= MaxBundleFiles {
			return Bundle{}, fmt.Errorf("skill %q exceeds bundle limits (%d bytes, %d files)", ref.Skill, MaxBundleBytes, MaxBundleFiles)
		}
		files[relative] = append([]byte(nil), content...)
	}
	if _, ok := files["SKILL.md"]; !ok {
		return Bundle{}, fmt.Errorf("skill %q is missing SKILL.md", ref.Skill)
	}
	if err := ValidatePortableSkill(files["SKILL.md"]); err != nil {
		return Bundle{}, fmt.Errorf("skill %q: %w", ref.Skill, err)
	}
	return Bundle{Ref: ref, Files: files, Digest: bundleDigest(files)}, nil
}

func excludedSkillFile(name string, pluginRoot bool) bool {
	parts := strings.Split(name, "/")
	// Never transport plugin control files or nested agent configuration, even
	// if a plugin declares its own root as a skill directory.
	for _, part := range parts {
		switch part {
		case ".git", ".claude-plugin", ".claude", ".agents", ".mcp.json", ".lsp.json":
			return true
		}
	}
	if len(parts) >= 2 && parts[len(parts)-2] == "hooks" && parts[len(parts)-1] == "hooks.json" {
		return true
	}
	if pluginRoot {
		switch parts[0] {
		case "hooks", "commands", "agents", "skills", "bin", "monitors", "output-styles", "workflows", "themes", "settings.json":
			return true
		}
	}
	return false
}

func bundleDigest(files map[string][]byte) string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	h := sha256.New()
	var size [8]byte
	for _, name := range names {
		binary.BigEndian.PutUint64(size[:], uint64(len(name)))
		h.Write(size[:])
		h.Write([]byte(name))
		binary.BigEndian.PutUint64(size[:], uint64(len(files[name])))
		h.Write(size[:])
		h.Write(files[name])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func validateSource(key string, source spec.SkillMarketplace) error {
	if !marketplaceKeyRE.MatchString(key) {
		return fmt.Errorf("invalid marketplace key %q", key)
	}
	if !repositoryRE.MatchString(source.Repository) || pathBase(source.Repository) == "." || pathBase(source.Repository) == ".." {
		return fmt.Errorf("marketplace repository must be a GitHub owner/repo without a URL or traversal")
	}
	if !commitRE.MatchString(source.Ref) {
		return fmt.Errorf("marketplace ref must be an immutable full lowercase 40-character Git commit SHA")
	}
	return nil
}

func pathBase(value string) string {
	parts := strings.Split(value, "/")
	return parts[len(parts)-1]
}

func (c *Catalog) repository(ctx context.Context, source spec.SkillMarketplace) (*cachedRepository, error) {
	key := strings.ToLower(source.Repository) + "@" + strings.ToLower(source.Ref)
	c.mu.Lock()
	if repo := c.cache[key]; repo != nil {
		c.tick++
		repo.used = c.tick
		c.mu.Unlock()
		return repo, nil
	}
	if pending := c.loading[key]; pending != nil {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-pending.done:
			return pending.repo, pending.err
		}
	}
	pending := &repositoryLoad{done: make(chan struct{})}
	c.loading[key] = pending
	c.mu.Unlock()
	select {
	case c.fetchSlots <- struct{}{}:
		pending.repo, pending.err = c.download(ctx, source)
		<-c.fetchSlots
	case <-ctx.Done():
		pending.err = ctx.Err()
	}
	c.mu.Lock()
	if pending.err == nil {
		for len(c.cache) >= maxCacheEntries || c.cacheBytes+pending.repo.size > maxCacheBytes {
			var oldest string
			var oldestTick uint64 = ^uint64(0)
			for k, repo := range c.cache {
				if repo.used < oldestTick {
					oldest, oldestTick = k, repo.used
				}
			}
			if oldest == "" {
				break
			}
			c.cacheBytes -= c.cache[oldest].size
			delete(c.cache, oldest)
		}
		c.tick++
		pending.repo.used = c.tick
		c.cache[key] = pending.repo
		c.cacheBytes += pending.repo.size
	}
	delete(c.loading, key)
	close(pending.done)
	c.mu.Unlock()
	return pending.repo, pending.err
}
