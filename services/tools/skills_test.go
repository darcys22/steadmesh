package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/skills"
	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/services/store"
)

func TestSkillDescriptorsRespectModes(t *testing.T) {
	r := &Registry{}
	for _, tc := range []struct {
		mode             string
		reads, mutations bool
	}{{"", false, false}, {"explicit", true, false}, {"discover", true, false}, {"agent_choice", true, true}, {"unknown", false, false}} {
		sm := compile.SeatManifest{}
		if tc.mode != "" {
			sm.Skills = &spec.SkillPolicy{Mode: tc.mode}
		}
		for _, tool := range r.skillTools() {
			want := tc.reads
			if tool.mutating {
				want = tc.mutations
			}
			if tool.allowed(&sm, nil) != want {
				t.Fatalf("mode %s tool %s allowed=%v", tc.mode, tool.name, !want)
			}
			var schema map[string]any
			if err := json.Unmarshal([]byte(tool.schema), &schema); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestSkillReadPagesPreserveCompleteContentAndBoundEscaping(t *testing.T) {
	ref := spec.SkillRef{Marketplace: "m", Plugin: "p", Skill: "s"}
	content := strings.Repeat("é\x00<>&\"\n", 6000)
	bundle := skills.Bundle{Files: map[string][]byte{"SKILL.md": []byte(content)}, Digest: strings.Repeat("a", 64)}
	var complete strings.Builder
	offset := 0
	for {
		v, err := skillReadPage(ref, bundle, offset)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(v)
		if len(encoded) > MaxResultBytes {
			t.Fatalf("oversize result %d", len(encoded))
		}
		page := v.(map[string]any)
		text := page["content"].(string)
		if !utf8.ValidString(text) {
			t.Fatal("invalid UTF-8")
		}
		complete.WriteString(text)
		next, more := page["next_offset"]
		if !more {
			break
		}
		if next.(int) <= offset {
			t.Fatal("pagination did not advance")
		}
		offset = next.(int)
	}
	if complete.String() != content {
		t.Fatal("read lost or duplicated content")
	}
	for _, offset := range []int{-1, 1, len(content) + 1} {
		if _, err := skillReadPage(ref, bundle, offset); err == nil {
			t.Fatalf("accepted invalid offset %d", offset)
		}
	}
}

func TestSkillReadsDoNotDiscoverInExplicitMode(t *testing.T) {
	ref := spec.SkillRef{Marketplace: "m", Plugin: "p", Skill: "s"}
	source := spec.SkillMarketplace{Repository: "a/b", Ref: strings.Repeat("a", 40)}
	s := store.SkillsSnapshot{Manifest: compile.SeatManifest{Skills: &spec.SkillPolicy{Mode: "explicit", Marketplaces: []string{"m"}}, SkillMarketplaces: map[string]spec.SkillMarketplace{"m": source}}}
	if _, err := skillReadable(s, ref); err == nil {
		t.Fatal("explicit mode revealed unselected skill")
	}
	s.Manifest.Skills.Install = []spec.SkillRef{ref}
	if got, err := skillReadable(s, ref); err != nil || got != source {
		t.Fatalf("explicit selected read: %+v %v", got, err)
	}
	s.Manifest.Skills.Mode = "discover"
	s.Manifest.Skills.Install = nil
	if _, err := skillReadable(s, ref); err != nil {
		t.Fatal(err)
	}
}

func TestSkillToolsRejectUntrustedSourceAndPathsBeforeStore(t *testing.T) {
	r := &Registry{}
	for _, raw := range []string{`{"marketplace":"m","plugin":"p","skill":"s","repository":"attacker/other"}`, `{"marketplace":"m","plugin":"p","skill":"../s"}`, `{"marketplace":"m","plugin":"p"}`, `{"marketplace":"m","plugin":"p","skill":"s","seat_id":"other"}`} {
		c := &Call{Args: json.RawMessage(raw)}
		if _, err := r.skillsInstall(context.Background(), c); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

type oversizedFileCatalog struct{}

func (oversizedFileCatalog) List(context.Context, string, spec.SkillMarketplace) ([]skills.Entry, error) {
	return nil, nil
}
func (oversizedFileCatalog) Resolve(_ context.Context, _ string, _ spec.SkillMarketplace, ref spec.SkillRef) (skills.Bundle, error) {
	files := map[string][]byte{}
	for i := 0; i < 512; i++ {
		files[fmt.Sprintf("f%d", i)] = []byte("x")
	}
	return skills.Bundle{Ref: ref, Files: files, Digest: strings.Repeat("a", 64)}, nil
}
func TestSkillsBundleAggregateFileLimit(t *testing.T) {
	r := &Registry{d: Deps{SkillCatalog: oversizedFileCatalog{}}}
	selected := make([]store.SkillSelection, 9)
	for i := range selected {
		selected[i] = store.SkillSelection{Ref: spec.SkillRef{Marketplace: "m", Plugin: "p", Skill: fmt.Sprintf("s%d", i)}, Owner: "agent"}
	}
	if _, err := r.resolveSkillsBundle(context.Background(), selected); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("aggregate file limit not enforced: %v", err)
	}
	if out, err := r.resolveSkillsBundle(context.Background(), selected[:8]); err != nil || len(out.Skills) != 8 {
		t.Fatalf("4096-file boundary: %d %v", len(out.Skills), err)
	}
}
