package skills

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/spec"
	"sigs.k8s.io/yaml"
)

// Snapshot is a freshly written, skill-only native loading tree. Original
// plugin manifests, hooks, MCP servers and installers are never copied. The
// owner must retain it while its harness may read it, then call Close.
type Snapshot struct {
	Root       string
	SkillDirs  []string
	PluginDirs []string
	Digest     string
	parent     *os.Root
	name       string
}

func (s *Snapshot) Close() error {
	if s == nil || s.parent == nil {
		return nil
	}
	err := s.parent.RemoveAll(s.name)
	closeErr := s.parent.Close()
	s.parent = nil
	if err != nil {
		return err
	}
	return closeErr
}

var nativeID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,127}$`)

// ValidatePortableSkill rejects Claude extensions that execute shell commands
// during loading or grant permissions. Skills remain untrusted instructions;
// supporting scripts can only be used via the seat's existing tool authority.
func ValidatePortableSkill(data []byte) error {
	if !utf8.Valid(data) {
		return fmt.Errorf("SKILL.md is not valid UTF-8")
	}
	if len(data) == 0 {
		return fmt.Errorf("empty SKILL.md")
	}
	if bytes.Contains(data, []byte("!`")) {
		return fmt.Errorf("shell preprocessing (!` commands) is not supported in portable skills")
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if strings.TrimSpace(lines[0]) != "---" {
		return nil
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return fmt.Errorf("unterminated skill frontmatter")
	}
	var fields map[string]any
	if err := yaml.UnmarshalStrict([]byte(strings.Join(lines[1:end], "\n")), &fields); err != nil {
		return fmt.Errorf("invalid skill frontmatter: %w", err)
	}
	for _, k := range []string{"hooks", "allowed-tools"} {
		if _, ok := fields[k]; ok {
			return fmt.Errorf("skill frontmatter %q is not supported; skills cannot add hooks or grant tools", k)
		}
	}
	return nil
}

// Materialize validates a complete response before exposing it to a harness.
// Each call creates a unique tree; a failure never changes a previous tree.
// Writes use os.Root so even a concurrent symlink replacement cannot escape.
func Materialize(baseDir string, bundles []runtimeapi.SkillBundle) (_ *Snapshot, err error) {
	if len(bundles) > 256 {
		return nil, fmt.Errorf("too many skills (maximum 256)")
	}
	ordered := append([]runtimeapi.SkillBundle(nil), bundles...)
	sort.Slice(ordered, func(i, j int) bool { return bundleKey(ordered[i]) < bundleKey(ordered[j]) })
	seen := map[string]bool{}
	total, count := 0, 0
	hash := sha256.New()
	for _, b := range ordered {
		if !nativeID.MatchString(b.Ref.Plugin) || !nativeID.MatchString(b.Ref.Skill) || b.Ref.Marketplace == "" {
			return nil, fmt.Errorf("invalid skill reference")
		}
		key := bundleKey(b)
		if seen[key] {
			return nil, fmt.Errorf("duplicate skill reference %s", key)
		}
		seen[key] = true
		if err := ValidatePortableSkill(b.Files["SKILL.md"]); err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		meta, _ := json.Marshal(struct{ Key, Repository, Ref string }{key, b.Source.Repository, b.Source.Ref})
		writeHashPart(hash, meta)
		for _, name := range sortedFileNames(b.Files) {
			if !fs.ValidPath(name) || strings.ContainsAny(name, "\\\x00") || strings.Contains(name, ":") || name == "." {
				return nil, fmt.Errorf("unsafe skill file path %q", name)
			}
			if name != "SKILL.md" && strings.EqualFold(path.Base(name), "SKILL.md") {
				return nil, fmt.Errorf("nested skill entry %q is not allowed", name)
			}
			total += len(b.Files[name])
			count++
			if total > runtimeapi.MaxSkillsBundleBytes || count > runtimeapi.MaxSkillsBundleFiles {
				return nil, fmt.Errorf("skill snapshot exceeds 16 MiB or 4096 files")
			}
			writeHashPart(hash, []byte(name))
			writeHashPart(hash, b.Files[name])
		}
	}
	if err := os.MkdirAll(baseDir, 0700); err != nil {
		return nil, err
	}
	parent, err := os.OpenRoot(baseDir)
	if err != nil {
		return nil, err
	}
	rootPath, err := os.MkdirTemp(baseDir, "skill-snapshot-")
	if err != nil {
		parent.Close()
		return nil, err
	}
	snap := &Snapshot{Root: rootPath, name: filepath.Base(rootPath), parent: parent, Digest: hex.EncodeToString(hash.Sum(nil)), SkillDirs: []string{}, PluginDirs: []string{}}
	defer func() {
		if err != nil {
			_ = snap.Close()
		}
	}()
	root, err := parent.OpenRoot(snap.name)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	plugins := map[string]bool{}
	for _, b := range ordered {
		// Hash the pair to keep names stable, short and collision-free even when
		// marketplace and plugin names contain the same separators.
		sum := sha256.Sum256([]byte(b.Ref.Marketplace + "\x00" + b.Ref.Plugin))
		plugin := "steadmesh-" + hex.EncodeToString(sum[:16])
		if !plugins[plugin] {
			plugins[plugin] = true
			manifest, _ := json.Marshal(map[string]string{"name": plugin})
			if err = writeSnapshotFile(root, path.Join(plugin, ".claude-plugin/plugin.json"), manifest); err != nil {
				return nil, err
			}
			snap.PluginDirs = append(snap.PluginDirs, filepath.Join(rootPath, plugin))
		}
		dir := path.Join(plugin, "skills", NativeSkillName(b.Ref))
		for _, name := range sortedFileNames(b.Files) {
			data := b.Files[name]
			if name == "SKILL.md" {
				data, err = portableSkillContent(b.Ref, data)
				if err != nil {
					return nil, err
				}
			}
			if err = writeSnapshotFile(root, path.Join(dir, name), data); err != nil {
				return nil, err
			}
		}
		snap.SkillDirs = append(snap.SkillDirs, filepath.Join(rootPath, filepath.FromSlash(dir)))
	}
	return snap, nil
}

func bundleKey(b runtimeapi.SkillBundle) string {
	return b.Ref.Marketplace + "/" + b.Ref.Plugin + "/" + b.Ref.Skill
}
func sortedFileNames(files map[string][]byte) []string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
func writeHashPart(h interface{ Write([]byte) (int, error) }, p []byte) {
	fmt.Fprintf(h, "%d:", len(p))
	_, _ = h.Write(p)
}
func writeSnapshotFile(root *os.Root, name string, data []byte) error {
	if err := root.MkdirAll(path.Dir(name), 0700); err != nil {
		return err
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	cerr := f.Close()
	if err != nil {
		return err
	}
	return cerr
}

// NativeSkillName avoids native frontmatter-name collisions across marketplaces
// and plugins. A pin update preserves the name, but changes the snapshot digest.
func NativeSkillName(ref spec.SkillRef) string {
	sum := sha256.Sum256([]byte(ref.Marketplace + "\x00" + ref.Plugin + "\x00" + ref.Skill))
	prefix := ref.Skill
	if len(prefix) > 32 {
		prefix = prefix[:32]
	}
	return "sm-" + prefix + "-" + hex.EncodeToString(sum[:8])
}

func portableSkillContent(ref spec.SkillRef, data []byte) ([]byte, error) {
	if err := ValidatePortableSkill(data); err != nil {
		return nil, err
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	fields := map[string]any{}
	body := text
	if strings.TrimSpace(lines[0]) == "---" {
		end := 1
		for ; end < len(lines); end++ {
			if strings.TrimSpace(lines[end]) == "---" {
				break
			}
		}
		if err := yaml.UnmarshalStrict([]byte(strings.Join(lines[1:end], "\n")), &fields); err != nil {
			return nil, err
		}
		if fields == nil {
			fields = map[string]any{}
		}
		body = strings.Join(lines[end+1:], "\n")
	}
	fields["name"] = NativeSkillName(ref)
	if desc, ok := fields["description"].(string); !ok || strings.TrimSpace(desc) == "" {
		fields["description"] = "Marketplace skill " + ref.Marketplace + "/" + ref.Plugin + "/" + ref.Skill
	}
	header, err := yaml.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return []byte("---\n" + string(header) + "---\n" + body), nil
}
