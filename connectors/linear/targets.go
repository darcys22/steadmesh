package linear

import (
	"encoding/json"

	"github.com/darcys22/steadmesh/connectors"
)

var _ connectors.TargetResolver = (*Adapter)(nil)

// OperationTargets uses the same typed parameters and defaults as execution.
// A list/search across the account has no concrete target. Mutations touching
// several teams, or assigning a task to a project, require all those resources
// as well as the task's own ID (or team, when creating it).
func (a *Adapter) OperationTargets(op string, raw json.RawMessage) ([]string, error) {
	switch op {
	case OpProjectRead:
		var p projectReadParams
		if err := decode(raw, &p); err != nil {
			return nil, err
		}
		if err := p.validate(); err != nil {
			return nil, err
		}
		if p.ID != "" {
			return []string{p.ID}, nil
		}
		return nil, nil
	case OpProjectCreate:
		p, err := a.parseProjectCreate(raw)
		return p.TeamIDs, err
	case OpTaskRead:
		var p taskReadParams
		if err := decode(raw, &p); err != nil {
			return nil, err
		}
		if err := p.validate(); err != nil {
			return nil, err
		}
		if p.ID != "" {
			return []string{p.ID}, nil
		}
		if p.ProjectID != "" {
			return []string{p.ProjectID}, nil
		}
		return nil, nil
	case OpTaskWrite:
		var p taskWriteParams
		if err := decode(raw, &p); err != nil {
			return nil, err
		}
		if err := p.validate(a.teamID); err != nil {
			return nil, err
		}
		target := p.ID
		if target == "" {
			target = p.TeamID
		}
		targets := []string{target}
		if p.ProjectID != nil && *p.ProjectID != "" {
			targets = append(targets, *p.ProjectID)
		}
		return targets, nil
	case OpCommentRead:
		var p commentReadParams
		if err := decode(raw, &p); err != nil {
			return nil, err
		}
		if err := p.validate(); err != nil {
			return nil, err
		}
		return []string{p.IssueID}, nil
	case OpCommentWrite:
		p, err := parseCommentWrite(raw)
		if err != nil {
			return nil, err
		}
		return []string{p.IssueID}, nil
	default:
		return nil, invalid("unsupported operation %q", op)
	}
}
