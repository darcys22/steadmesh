package skills

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/darcys22/steadmesh/pkg/spec"
	"go.yaml.in/yaml/v3"
)

const maxManifestBytes = 1 << 20

type skillRecord struct {
	entry      Entry
	directory  string
	pluginRoot string
}

type marketplaceIndex struct {
	files       map[string][]byte
	skills      map[string]skillRecord
	unsupported map[string]string
	filenames   []string
	children    map[string][]string
}

func (c *Catalog) index(ctx context.Context, key string, source spec.SkillMarketplace) (*marketplaceIndex, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateSource(key, source); err != nil {
		return nil, err
	}
	repository, err := c.repository(ctx, source)
	if err != nil {
		return nil, err
	}
	manifestBytes, ok := repository.files[".claude-plugin/marketplace.json"]
	if !ok {
		return nil, fmt.Errorf("marketplace is missing .claude-plugin/marketplace.json")
	}
	manifest, err := jsonObject(manifestBytes)
	if err != nil {
		return nil, fmt.Errorf("marketplace manifest: %w", err)
	}
	name, err := requiredString(manifest, "name")
	if err != nil {
		return nil, fmt.Errorf("marketplace manifest: %w", err)
	}
	if !identifierRE.MatchString(name) {
		return nil, fmt.Errorf("marketplace manifest has invalid name %q", name)
	}
	owner, err := jsonObject(manifest["owner"])
	if err != nil {
		return nil, fmt.Errorf("marketplace owner: %w", err)
	}
	if _, err := requiredString(owner, "name"); err != nil {
		return nil, fmt.Errorf("marketplace owner: %w", err)
	}
	var plugins []json.RawMessage
	if raw := manifest["plugins"]; len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, fmt.Errorf("marketplace plugins must be an array")
	}
	if err := json.Unmarshal(manifest["plugins"], &plugins); err != nil {
		return nil, fmt.Errorf("marketplace plugins: %w", err)
	}
	if len(plugins) > 512 {
		return nil, fmt.Errorf("marketplace exceeds 512 plugins")
	}
	index := &marketplaceIndex{files: repository.files, skills: make(map[string]skillRecord), unsupported: make(map[string]string), children: make(map[string][]string)}
	// Index direct skill children once. Repeated declared paths must not cause
	// a full archive scan each time (manifests and their contents are untrusted).
	for filename := range index.files {
		index.filenames = append(index.filenames, filename)
		if path.Base(filename) == "SKILL.md" {
			directory := path.Dir(filename)
			parent := path.Dir(directory)
			index.children[parent] = append(index.children[parent], directory)
		}
	}
	sort.Strings(index.filenames)
	seenPlugins := make(map[string]bool)
	for _, raw := range plugins {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		plugin, err := jsonObject(raw)
		if err != nil {
			return nil, fmt.Errorf("marketplace plugin: %w", err)
		}
		name, err := requiredString(plugin, "name")
		if err != nil || !identifierRE.MatchString(name) {
			return nil, fmt.Errorf("marketplace plugin has invalid or missing name")
		}
		if seenPlugins[name] {
			return nil, fmt.Errorf("duplicate marketplace plugin %q", name)
		}
		seenPlugins[name] = true
		if err := index.addPlugin(key, name, plugin); err != nil {
			return nil, fmt.Errorf("plugin %q: %w", name, err)
		}
		if len(index.skills) > 4096 {
			return nil, fmt.Errorf("marketplace exceeds 4096 skills")
		}
	}
	return index, nil
}

func (index *marketplaceIndex) addPlugin(key, name string, entry map[string]json.RawMessage) error {
	// Component paths and strict-mode merging follow the official references:
	// https://code.claude.com/docs/en/plugins-reference#marketplace-entries-and-the-manifest
	// https://code.claude.com/docs/en/plugins-reference#how-each-key-combines-with-its-default-location
	// We read only the skills subset and never enable other plugin components.
	strict := true
	if raw, exists := entry["strict"]; exists {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("strict must be a boolean")
		}
		if err := json.Unmarshal(raw, &strict); err != nil {
			return fmt.Errorf("strict must be a boolean")
		}
	}
	raw, exists := entry["source"]
	if !exists {
		return fmt.Errorf("source is required")
	}
	var source string
	if err := json.Unmarshal(raw, &source); err != nil {
		object, objectErr := jsonObject(raw)
		if objectErr != nil {
			return fmt.Errorf("source must be a relative path or a source object")
		}
		typeName, typeErr := requiredString(object, "source")
		if typeErr != nil {
			return fmt.Errorf("source object: %w", typeErr)
		}
		index.unsupported[name] = fmt.Sprintf("source type %q is not supported; only relative paths within the pinned marketplace repository are allowed", typeName)
		return nil
	}
	root, err := relativeDirectory(source)
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}
	if !index.hasDirectory(root) {
		return fmt.Errorf("source directory %q does not exist", source)
	}
	entrySkills, err := skillPaths(entry)
	if err != nil {
		return err
	}
	plugin := entry
	manifestSkills := []string(nil)
	if manifestBytes, exists := index.files[path.Join(root, ".claude-plugin/plugin.json")]; exists {
		plugin, err = jsonObject(manifestBytes)
		if err != nil {
			return fmt.Errorf("plugin manifest: %w", err)
		}
		manifestName, err := requiredString(plugin, "name")
		if err != nil || !identifierRE.MatchString(manifestName) {
			return fmt.Errorf("plugin manifest has invalid or missing name")
		}
		if !strict {
			for _, component := range []string{"commands", "agents", "skills", "hooks", "outputStyles", "themes"} {
				if _, exists := entry[component]; exists {
					return fmt.Errorf("strict:false entry has conflicting manifests (%s declared in marketplace entry)", component)
				}
			}
		}
		manifestSkills, err = skillPaths(plugin)
		if err != nil {
			return fmt.Errorf("plugin manifest: %w", err)
		}
	}
	description, err := optionalString(plugin, "description")
	if err != nil {
		return err
	}
	// Claude's root-source entries use explicit skill paths to select a subset.
	// Elsewhere, both manifest and marketplace paths add to the default scan.
	directories := make(map[string]bool)
	if !(root == "." && len(entrySkills) > 0) {
		defaultDir := path.Join(root, "skills")
		if index.hasDirectory(defaultDir) {
			directories[defaultDir] = true
		}
	}
	for _, relative := range append(manifestSkills, entrySkills...) {
		directory := path.Join(root, relative)
		if !index.hasDirectory(directory) {
			return fmt.Errorf("skills directory %q does not exist", relative)
		}
		directories[directory] = true
	}
	if len(entrySkills) == 0 && len(manifestSkills) == 0 && !index.hasDirectory(path.Join(root, "skills")) {
		if _, exists := index.files[path.Join(root, "SKILL.md")]; exists {
			directories[root] = true
		}
	}
	for _, directory := range sortedKeys(directories) {
		found := make(map[string]bool)
		if _, exists := index.files[path.Join(directory, "SKILL.md")]; exists {
			found[directory] = true
		} else {
			for _, child := range index.children[directory] {
				found[child] = true
			}
		}
		for _, skillDir := range sortedKeys(found) {
			skillName := path.Base(skillDir)
			if skillDir == "." {
				skillName = name
			}
			if !identifierRE.MatchString(skillName) {
				return fmt.Errorf("invalid skill directory identifier %q", skillName)
			}
			id := name + "/" + skillName
			if previous, exists := index.skills[id]; exists {
				if previous.directory == skillDir {
					continue
				}
				return fmt.Errorf("ambiguous skill %q appears in %q and %q", skillName, previous.directory, skillDir)
			}
			skillDescription, err := readDescription(index.files[path.Join(skillDir, "SKILL.md")])
			if err != nil {
				return fmt.Errorf("skill %q: %w", skillName, err)
			}
			if skillDescription == "" {
				skillDescription = description
			}
			index.skills[id] = skillRecord{entry: Entry{Ref: spec.SkillRef{Marketplace: key, Plugin: name, Skill: skillName}, Description: skillDescription}, directory: skillDir, pluginRoot: root}
		}
	}
	return nil
}

func relativeDirectory(value string) (string, error) {
	if value == "." || value == "./" {
		return ".", nil
	}
	if !strings.HasPrefix(value, "./") {
		return "", fmt.Errorf("path %q must start with ./ and remain inside its repository or plugin", value)
	}
	value = strings.TrimSuffix(strings.TrimPrefix(value, "./"), "/")
	if err := safeArchivePath(value); err != nil {
		return "", fmt.Errorf("path %q: %w", value, err)
	}
	return value, nil
}

func skillPaths(object map[string]json.RawMessage) ([]string, error) {
	raw, exists := object["skills"]
	if !exists {
		return nil, nil
	}
	var values []string
	var value string
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, fmt.Errorf("skills must be a path or array of paths, not null")
	}
	if err := json.Unmarshal(raw, &value); err == nil {
		values = []string{value}
	} else if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("skills must be a path or array of paths")
	}
	if len(values) > 512 {
		return nil, fmt.Errorf("skills exceeds 512 paths")
	}
	for i, value := range values {
		normalized, err := relativeDirectory(value)
		if err != nil {
			return nil, fmt.Errorf("skills: %w", err)
		}
		values[i] = normalized
	}
	return values, nil
}

func (index *marketplaceIndex) hasDirectory(directory string) bool {
	if directory == "." {
		return true
	}
	prefix := directory + "/"
	i := sort.SearchStrings(index.filenames, prefix)
	return i < len(index.filenames) && strings.HasPrefix(index.filenames[i], prefix)
}

func sortedKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func requiredString(object map[string]json.RawMessage, key string) (string, error) {
	value, err := optionalString(object, key)
	if err != nil {
		return "", err
	}
	if value == "" {
		return "", fmt.Errorf("%s is required and must be a nonempty string", key)
	}
	return value, nil
}

func optionalString(object map[string]json.RawMessage, key string) (string, error) {
	raw, exists := object[key]
	if !exists {
		return "", nil
	}
	var value string
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", fmt.Errorf("%s must be a string", key)
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("%s must be a string", key)
	}
	if len(value) > 8192 {
		return "", fmt.Errorf("%s exceeds 8192 bytes", key)
	}
	return value, nil
}

// Go's standard JSON decoder accepts duplicate object keys. Reject them to
// avoid different consumers resolving the same declaration differently.
func jsonObject(data []byte) (map[string]json.RawMessage, error) {
	if len(data) == 0 || len(data) > maxManifestBytes {
		return nil, fmt.Errorf("JSON object is missing or exceeds %d bytes", maxManifestBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := validateJSONValue(decoder, 0); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing JSON data")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, fmt.Errorf("expected JSON object")
	}
	return object, nil
}

func validateJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 64 {
		return fmt.Errorf("JSON nesting exceeds 64 levels")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok {
				return fmt.Errorf("invalid JSON object key")
			}
			if seen[key] {
				return fmt.Errorf("duplicate JSON key %q", key)
			}
			seen[key] = true
			if err := validateJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := validateJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter")
	}
	_, err = decoder.Token()
	return err
}

func readDescription(data []byte) (string, error) {
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	if !bytes.HasPrefix(data, []byte("---\n")) {
		return "", nil
	}
	remaining := data[4:]
	end := bytes.Index(remaining, []byte("\n---\n"))
	if end < 0 && bytes.HasSuffix(remaining, []byte("\n---")) {
		end = len(remaining) - 4
	}
	if end < 0 || end > 64<<10 {
		return "", fmt.Errorf("invalid or oversized SKILL.md frontmatter")
	}
	var document yaml.Node
	if err := yaml.Unmarshal(remaining[:end+1], &document); err != nil {
		return "", fmt.Errorf("invalid SKILL.md frontmatter: %w", err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return "", fmt.Errorf("SKILL.md frontmatter must be a mapping")
	}
	fields := document.Content[0].Content
	seen := make(map[string]bool)
	description := ""
	for i := 0; i < len(fields); i += 2 {
		key, value := fields[i], fields[i+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || seen[key.Value] {
			return "", fmt.Errorf("invalid or duplicate SKILL.md frontmatter field")
		}
		seen[key.Value] = true
		if key.Value == "name" || key.Value == "description" {
			if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
				return "", fmt.Errorf("SKILL.md %s must be a string", key.Value)
			}
			if key.Value == "name" && !identifierRE.MatchString(value.Value) {
				return "", fmt.Errorf("invalid SKILL.md name")
			}
			if key.Value == "description" {
				if len(value.Value) > 8192 {
					return "", fmt.Errorf("SKILL.md description exceeds 8192 bytes")
				}
				description = value.Value
			}
		}
	}
	return description, nil
}
