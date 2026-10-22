package api

import "net/http"

func (s *server) skillsBundle(w http.ResponseWriter, r *http.Request, q *seatReq) {
	// There are intentionally no source, URL, path or skill query parameters.
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "invalid", "skills bundle takes no query parameters")
		return
	}
	out, err := s.Tools.SkillsBundle(r.Context(), q.seat.OrganizationID, q.seat.ID)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}
