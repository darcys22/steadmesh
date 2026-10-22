package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSkillsBundleRejectsAgentSuppliedSources(t *testing.T) {
	for _, query := range []string{"?repository=attacker/skills", "?seat_id=other", "?skill=unselected", "?ref=main"} {
		w := httptest.NewRecorder()
		(&server{}).skillsBundle(w, httptest.NewRequest(http.MethodGet, "/v1/skills/bundle"+query, nil), nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("accepted %s: %d", query, w.Code)
		}
	}
}
