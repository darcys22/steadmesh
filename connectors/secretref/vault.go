package secretref

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
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
// Its own login token is renewed before it expires, falling back to a new
// login; that is separate from reloading the credentials stored in Vault,
// which the caller does by resolving again.
type Vault struct {
	addr    *url.URL
	role    string
	mount   string
	jwtPath string
	http    *http.Client
	now     func() time.Time

	mu        sync.Mutex
	token     string
	renewable bool
	lease     time.Duration
	expires   time.Time // zero means no expiry
	status    LoginStatus
}

// LoginStatus describes the platform's own Vault login. It holds no secrets.
type LoginStatus struct {
	LoggedIn  bool      `json:"logged_in"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	LastLogin time.Time `json:"last_login,omitzero"`
	LastRenew time.Time `json:"last_renew,omitzero"`
	LastError string    `json:"last_error,omitempty"`
}

// LoginStatus reports the state of the platform's Vault login.
func (v *Vault) LoginStatus() LoginStatus {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.status
}

// NewVault builds a resolver for "vault:<path>". mount defaults to "secret"
// and jwtPath to the in-cluster ServiceAccount token.
func NewVault(addr, role, mount, jwtPath string) (*Vault, error) {
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

// Resolve implements connectors.Secrets. Version is the KV v2 version that
// was read.
func (v *Vault) Resolve(ctx context.Context, ref string) (connectors.Resolved, error) {
	path, err := expect(ref, SchemeVault)
	if err != nil {
		return connectors.Resolved{}, err
	}
	path = strings.Trim(path, "/")
	for _, seg := range strings.Split(path, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return connectors.Resolved{}, fmt.Errorf("%w: invalid vault path %q", connectors.ErrPermanent, path)
		}
	}
	for attempt := 0; ; attempt++ {
		token, err := v.loginToken(ctx)
		if err != nil {
			return connectors.Resolved{}, err
		}
		data, version, status, err := v.read(ctx, token, path)
		if status == http.StatusForbidden && attempt == 0 {
			// The token may have been revoked early; log in again once.
			v.invalidate(token)
			continue
		}
		if err != nil {
			return connectors.Resolved{}, err
		}
		return resolved(data, version), nil
	}
}

func (v *Vault) invalidate(token string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.token == token {
		v.token = ""
		v.status.LoggedIn = false
	}
}

// renewBefore is how long before expiry the login is renewed.
func renewBefore(lease time.Duration) time.Duration { return min(lease/10, 30*time.Second) }

// loginToken returns a valid login token: the current one, a renewed one
// when it is about to expire, or a new login.
func (v *Vault) loginToken(ctx context.Context) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now()
	if v.token != "" && (v.expires.IsZero() || now.Before(v.expires)) {
		return v.token, nil
	}
	if v.token != "" && v.renewable {
		if err := v.renewLocked(ctx); err == nil {
			return v.token, nil
		}
	}
	return v.loginLocked(ctx)
}

func (v *Vault) renewLocked(ctx context.Context) error {
	var out struct {
		Auth *struct {
			LeaseDuration int64 `json:"lease_duration"`
			Renewable     bool  `json:"renewable"`
		} `json:"auth"`
	}
	if _, err := v.call(ctx, http.MethodPost, "/v1/auth/token/renew-self", v.token, []byte(`{}`), &out); err != nil {
		v.status.LastError = "renew: " + err.Error()
		return err
	}
	if out.Auth == nil || out.Auth.LeaseDuration <= 0 {
		v.status.LastError = "renew: no lease returned"
		return errors.New("vault renew returned no lease")
	}
	v.setLease(time.Duration(out.Auth.LeaseDuration)*time.Second, out.Auth.Renewable)
	v.status.LastRenew = v.now()
	v.status.LastError = ""
	return nil
}

func (v *Vault) loginLocked(ctx context.Context) (string, error) {
	fail := func(err error) (string, error) {
		v.token = ""
		v.status.LoggedIn = false
		v.status.LastError = "login: " + err.Error()
		return "", err
	}
	// Read the token on every login: projected tokens rotate.
	jwt, err := os.ReadFile(v.jwtPath)
	if err != nil {
		return fail(fmt.Errorf("%w: reading service account token: %v", connectors.ErrUnauthorized, err))
	}
	body, _ := json.Marshal(map[string]string{"role": v.role, "jwt": strings.TrimSpace(string(jwt))})
	var out struct {
		Auth *struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int64  `json:"lease_duration"`
			Renewable     bool   `json:"renewable"`
		} `json:"auth"`
	}
	status, err := v.call(ctx, http.MethodPost, "/v1/auth/kubernetes/login", "", body, &out)
	if err != nil {
		return fail(fmt.Errorf("vault login: %w", err))
	}
	if out.Auth == nil || out.Auth.ClientToken == "" {
		return fail(fmt.Errorf("%w: vault login returned no token (status %d)", connectors.ErrUnauthorized, status))
	}
	v.token = out.Auth.ClientToken
	v.setLease(time.Duration(out.Auth.LeaseDuration)*time.Second, out.Auth.Renewable)
	v.status.LoggedIn = true
	v.status.LastLogin = v.now()
	v.status.LastError = ""
	return v.token, nil
}

// setLease records the login lease; zero means it never expires.
func (v *Vault) setLease(lease time.Duration, renewable bool) {
	v.lease, v.renewable = lease, renewable
	v.expires = time.Time{}
	v.status.ExpiresAt = time.Time{}
	if lease > 0 {
		v.status.ExpiresAt = v.now().Add(lease)
		v.expires = v.status.ExpiresAt.Add(-renewBefore(lease))
	}
}

func (v *Vault) read(ctx context.Context, token, path string) (map[string]string, string, int, error) {
	var out struct {
		Data *struct {
			Data     map[string]any `json:"data"`
			Metadata *struct {
				Version int64 `json:"version"`
			} `json:"metadata"`
		} `json:"data"`
	}
	status, err := v.call(ctx, http.MethodGet, "/v1/"+v.mount+"/data/"+path, token, nil, &out)
	if err != nil {
		return nil, "", status, fmt.Errorf("vault read %s: %w", path, err)
	}
	if out.Data == nil || out.Data.Data == nil {
		return nil, "", status, fmt.Errorf("%w: vault secret %s has no data (deleted version?)", connectors.ErrPermanent, path)
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
	version := ""
	if out.Data.Metadata != nil {
		version = strconv.FormatInt(out.Data.Metadata.Version, 10)
	}
	return res, version, status, nil
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
