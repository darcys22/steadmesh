package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

// SkillsBundle fetches the complete currently authorized skill set. Callers
// must fail closed on errors instead of retaining revoked managed skills.
func (c *Client) SkillsBundle(ctx context.Context) (runtimeapi.SkillsBundleResponse, error) {
	var out runtimeapi.SkillsBundleResponse
	resp, err := c.Raw(ctx, http.MethodGet, runtimeapi.PathSkillsBundle, nil, nil)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, decodeError(resp)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, runtimeapi.MaxSkillsBundleBytes+1))
	if err != nil {
		return out, err
	}
	if len(raw) > runtimeapi.MaxSkillsBundleBytes {
		return out, fmt.Errorf("skills bundle exceeds %d bytes", runtimeapi.MaxSkillsBundleBytes)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("decode skills bundle: %w", err)
	}
	if out.Skills == nil || len(out.Skills) > runtimeapi.MaxSkillBundles {
		return runtimeapi.SkillsBundleResponse{}, fmt.Errorf("invalid skills bundle set")
	}
	return out, nil
}
