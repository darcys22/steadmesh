package store

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBranchPreviewPreservesUTF8(t *testing.T) {
	for _, test := range []struct {
		text  string
		limit int
		want  string
		cut   bool
	}{
		{"short", 8, "short", false}, {"exact", 5, "exact", false}, {strings.Repeat("é", 4), 5, "éé", true}, {"a€b", 2, "a", true},
	} {
		got, cut := branchPreview(test.text, test.limit)
		if got != test.want || cut != test.cut || !utf8.ValidString(got) {
			t.Errorf("preview(%q,%d) = %q,%v", test.text, test.limit, got, cut)
		}
	}
}
