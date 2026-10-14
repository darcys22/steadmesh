package credentials

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/darcys22/steadmesh/connectors"
)

type credentialPayload struct {
	Token      string                           `json:"token"`
	Revocation *connectors.CredentialRevocation `json:"revocation,omitempty"`
}

// revokePersisted uses only the authenticated, encrypted issuance snapshot.
// GitHub invalidates an installation token using that token itself, without
// the original App private key or a currently configured connection.
func revokePersisted(ctx context.Context, route connectors.CredentialRevocation, token string) error {
	if route.Kind != "github_installation" {
		return errors.New("unsupported persisted credential revocation kind")
	}
	u, err := url.Parse(route.Endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return errors.New("invalid persisted credential revocation endpoint")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, strings.TrimSuffix(route.Endpoint, "/")+"/installation/token", nil)
	if err != nil {
		return errors.New("invalid persisted credential revocation request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return errors.New("provider credential revocation request failed")
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNoContent || res.StatusCode == http.StatusUnauthorized {
		return nil
	}
	return errors.New("provider credential revocation was not confirmed")
}
