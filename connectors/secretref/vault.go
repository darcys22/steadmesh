package secretref

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/darcys22/steadmesh/connectors"
)

// DefaultJWTPath is the default ServiceAccount token location.
const DefaultJWTPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// DefaultMount is the default KV v2 mount.
const DefaultMount = "secret"

const maxVaultResponse = 1 << 20

// Vault reads KV v2 secrets after logging in with the Kubernetes auth method.
type Vault struct {
	addr    *url.URL
	role    string
	mount   string
	jwtPath string
	http    *http.Client
	now     func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time // zero means no expiry
}

// NewVault builds a resolver for "vault:<path>". mount defaults to "secret"
// and jwtPath to the in-cluster ServiceAccount token.
func NewVault(addr, role, mount, jwtPath string) (connectors.Secrets, error) {
	u, err := url.Parse(addr)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%w: invalid vault address %q", connectors.ErrPermanent, addr)
	}
	if role == "" {
		return nil, fmt.Errorf("%w: vault role is required", connectors.ErrPermanent)
	}
	if mount = strings.Trim(mount, "/"); mount == "" {
		mount = DefaultMount
	}
	if jwtPath == "" {
		jwtPath = DefaultJWTPath
	}
	return &Vault{
		addr: u, role: role, mount: mount, jwtPath: jwtPath,
		http: &http.Client{Timeout: 15 * time.Second}, now: time.Now,
	}, nil
}

// Resolve implements connectors.Secrets.
func (v *Vault) Resolve(ctx context.Context, ref string) (map[string]string, error) {
	path, err := expect(ref, SchemeVault)
	if err != nil {
		return nil, err
	}
	path = strings.Trim(path, "/")
	for _, seg := range strings.Split(path, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return nil, fmt.Errorf("%w: invalid vault path %q", connectors.ErrPermanent, path)
		}
	}
	for attempt := 0; ; attempt++ {
		token, err := v.loginToken(ctx)
		if err != nil {
			return nil, err
		}
		data, status, err := v.read(ctx, token, path)
		if status == http.StatusForbidden && attempt == 0 {
			// The token may have been revoked early; log in again once.
			v.invalidate(token)
			continue
		}
		return data, err
	}
}

func (v *Vault) invalidate(token string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.token == token {
		v.token = ""
	}
}

func (v *Vault) loginToken(ctx context.Context) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.token != "" && (v.expires.IsZero() || v.now().Before(v.expires)) {
		return v.token, nil
	}
	// Read the token on every login: projected tokens rotate.
	jwt, err := os.ReadFile(v.jwtPath)
	if err != nil {
		return "", fmt.Errorf("%w: reading service account token: %v", connectors.ErrUnauthorized, err)
	}
	body, _ := json.Marshal(map[string]string{"role": v.role, "jwt": strings.TrimSpace(string(jwt))})
	var out struct {
		Auth *struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int64  `json:"lease_duration"`
		} `json:"auth"`
	}
	status, err := v.call(ctx, http.MethodPost, "/v1/auth/kubernetes/login", "", body, &out)
	if err != nil {
		return "", fmt.Errorf("vault login: %w", err)
	}
	if out.Auth == nil || out.Auth.ClientToken == "" {
		return "", fmt.Errorf("%w: vault login returned no token (status %d)", connectors.ErrUnauthorized, status)
	}
	v.token = out.Auth.ClientToken
	v.expires = time.Time{}
	if d := out.Auth.LeaseDuration; d > 0 {
		// Log in again a little before the lease ends.
		lease := time.Duration(d) * time.Second
		v.expires = v.now().Add(lease - min(lease/10, 30*time.Second))
	}
	return v.token, nil
}

func (v *Vault) read(ctx context.Context, token, path string) (map[string]string, int, error) {
	var out struct {
		Data *struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	status, err := v.call(ctx, http.MethodGet, "/v1/"+v.mount+"/data/"+path, token, nil, &out)
	if err != nil {
		return nil, status, fmt.Errorf("vault read %s: %w", path, err)
	}
	if out.Data == nil || out.Data.Data == nil {
		return nil, status, fmt.Errorf("%w: vault secret %s has no data (deleted version?)", connectors.ErrPermanent, path)
	}
	res := make(map[string]string, len(out.Data.Data))
	for k, val := range out.Data.Data {
		if s, ok := val.(string); ok {
			res[k] = s
			continue
		}
		b, _ := json.Marshal(val)
		res[k] = string(b)
	}
	return res, status, nil
}

// call performs one Vault request. Errors carry only the status, never bodies.
func (v *Vault) call(ctx context.Context, method, path, token string, body []byte, out any) (int, error) {
	u := v.addr.JoinPath(path)
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", connectors.ErrPermanent, err)
	}
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := v.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", connectors.ErrRetryable, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxVaultResponse))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("%w: reading response: %v", connectors.ErrRetryable, err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return resp.StatusCode, fmt.Errorf("%w: not found", connectors.ErrPermanent)
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized:
		return resp.StatusCode, fmt.Errorf("%w: permission denied (status %d)", connectors.ErrUnauthorized, resp.StatusCode)
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return resp.StatusCode, fmt.Errorf("%w: status %d", connectors.ErrRetryable, resp.StatusCode)
	case resp.StatusCode/100 != 2:
		return resp.StatusCode, fmt.Errorf("%w: status %d", connectors.ErrPermanent, resp.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return resp.StatusCode, fmt.Errorf("%w: decoding response", connectors.ErrRetryable)
	}
	return resp.StatusCode, nil
}
