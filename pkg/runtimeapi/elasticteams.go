package runtimeapi

import (
	"fmt"
	"strings"
)

const PathElasticTeams = "/v1/elastic-teams"

// ElasticTeamRequest admits the entire required team or no workers at all.
// Workers cannot alter templates or request an independent execution identity.
type ElasticTeamRequest struct {
	RequestID string                 `json:"request_id"`
	Workers   []ElasticWorkerRequest `json:"workers"`
}

type ElasticWorkerRequest struct {
	Name     string         `json:"name"`
	Template string         `json:"template"`
	Task     TaskRunRequest `json:"task"`
}

type ElasticTeam struct {
	ID                string              `json:"id"`
	RequestID         string              `json:"request_id"`
	ParentExecutionID string              `json:"parent_execution_id"`
	Workers           []ElasticTeamWorker `json:"workers"`
}

type ElasticTeamWorker struct {
	Name     string  `json:"name"`
	Template string  `json:"template"`
	Run      TaskRun `json:"run"`
}

func ValidateElasticTeam(req *ElasticTeamRequest) error {
	if len(req.Workers) < 1 || len(req.Workers) > 32 {
		return fmt.Errorf("workers must contain 1 to 32 members")
	}
	seen := map[string]bool{}
	for i := range req.Workers {
		w := &req.Workers[i]
		if strings.TrimSpace(w.Name) == "" || len(w.Name) > 80 || seen[w.Name] {
			return fmt.Errorf("worker names must be unique, nonempty and at most 80 bytes")
		}
		for _, ch := range w.Name {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
				return fmt.Errorf("worker names use only letters, digits, hyphens and underscores")
			}
		}
		seen[w.Name] = true
		if strings.TrimSpace(w.Template) == "" || len(w.Template) > 80 {
			return fmt.Errorf("template is required and limited to 80 bytes")
		}
		if w.Task.RequestID != "" || w.Task.Template != "" {
			return fmt.Errorf("worker task request_id and template are assigned by the server")
		}
		task := w.Task // Preserve omitted timeout until the committed policy is available.
		if err := ValidateTaskRun(&task); err != nil {
			return fmt.Errorf("worker %s: %w", w.Name, err)
		}
	}
	return nil
}
