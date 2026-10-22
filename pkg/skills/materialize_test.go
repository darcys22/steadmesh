package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/spec"
)

func testBundle() runtimeapi.SkillBundle {
	return runtimeapi.SkillBundle{Ref: spec.SkillRef{Marketplace: "example", Plugin: "documents", Skill: "pdf"}, Source: spec.SkillMarketplace{Repository: "owner/repo", Ref: strings.Repeat("a", 40)}, Files: map[string][]byte{"SKILL.md": []byte("---\nname: pdf\ndescription: Read a PDF.\n---\nUse the helper in scripts/read.py."), "scripts/read.py": []byte("# data, not executed")}}
}

func TestMaterializePortableNativeSnapshot(t *testing.T) {
	base := t.TempDir()
	b := testBundle()
	s, err := Materialize(base, []runtimeapi.SkillBundle{b})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.SkillDirs) != 1 || len(s.PluginDirs) != 1 || s.Digest == "" {
		t.Fatalf("snapshot %+v", s)
	}
	got, err := os.ReadFile(filepath.Join(s.SkillDirs[0], "SKILL.md"))
	if err != nil || !strings.Contains(string(got), NativeSkillName(b.Ref)) || !strings.Contains(string(got), "Use the helper") {
		t.Fatalf("content: %s %v", got, err)
	}
	info, err := os.Stat(filepath.Join(s.SkillDirs[0], "scripts/read.py"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("script permission: %v %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(s.PluginDirs[0], ".claude-plugin/plugin.json")); err != nil {
		t.Fatal(err)
	}
	s2, err := Materialize(base, []runtimeapi.SkillBundle{b})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s.Root == s2.Root || s.Digest != s2.Digest {
		t.Fatal("snapshots must be isolated and deterministic")
	}
	root := s.Root
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("snapshot retained")
	}
	if _, err := os.Stat(s2.Root); err != nil {
		t.Fatal("cleanup touched another snapshot")
	}
}

func TestMaterializeRejectsUnsafeBundles(t *testing.T) {
	for _, name := range []string{"../escape", "/absolute", "a\\b", "sub/SKILL.md", "SKILL.md/x", "script:stream"} {
		t.Run(name, func(t *testing.T) {
			b := testBundle()
			b.Files[name] = []byte("bad")
			if s, err := Materialize(t.TempDir(), []runtimeapi.SkillBundle{b}); err == nil {
				s.Close()
				t.Fatal("accepted unsafe bundle")
			}
		})
	}
	for _, content := range []string{"", "---\nhooks: {}\n---\ntext", "---\nallowed-tools: Bash\n---\ntext", "!`touch /tmp/unwanted`", "---\nname: test", "---\nname: first\nname: second\n---\ntext"} {
		t.Run(content, func(t *testing.T) {
			b := testBundle()
			b.Files["SKILL.md"] = []byte(content)
			if s, err := Materialize(t.TempDir(), []runtimeapi.SkillBundle{b}); err == nil {
				s.Close()
				t.Fatal("accepted executable or malformed skill")
			}
		})
	}
	b := testBundle()
	if _, err := Materialize(t.TempDir(), []runtimeapi.SkillBundle{b, b}); err == nil {
		t.Fatal("duplicate accepted")
	}
}

func TestMaterializeEmptyAndDigestChanges(t *testing.T) {
	base := t.TempDir()
	empty, err := Materialize(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	if len(empty.SkillDirs) != 0 || len(empty.PluginDirs) != 0 || empty.Digest == "" {
		t.Fatal("bad empty snapshot")
	}
	b := testBundle()
	one, err := Materialize(base, []runtimeapi.SkillBundle{b})
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	b.Files["scripts/read.py"] = []byte("changed")
	two, err := Materialize(base, []runtimeapi.SkillBundle{b})
	if err != nil {
		t.Fatal(err)
	}
	defer two.Close()
	if one.Digest == two.Digest || empty.Digest == one.Digest {
		t.Fatal("digest ignores content")
	}
}

func TestSnapshotCloseDoesNotFollowSymlink(t *testing.T) {
	base := t.TempDir()
	s, err := Materialize(base, []runtimeapi.SkillBundle{testBundle()})
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "keep")
	if err := os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(s.Root); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, s.Root); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatal("cleanup followed symlink")
	}
}

func TestMaterializeNamespacedAcrossSources(t *testing.T) {
	a, b := testBundle(), testBundle()
	b.Ref.Plugin = "other-plugin"
	snap, err := Materialize(t.TempDir(), []runtimeapi.SkillBundle{a, b})
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	if NativeSkillName(a.Ref) == NativeSkillName(b.Ref) || len(NativeSkillName(a.Ref)) > 64 {
		t.Fatal("native names collide or exceed standard")
	}
	for _, dir := range snap.SkillDirs {
		data, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "name: "+filepath.Base(dir)) {
			t.Fatalf("name/dir mismatch: %s", data)
		}
	}
}

func TestPortableSkillRequiresUTF8(t *testing.T) {
	if err := ValidatePortableSkill([]byte{0xff}); err == nil {
		t.Fatal("accepted invalid UTF-8")
	}
}
