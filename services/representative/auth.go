package representative

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

var (
	ErrUnauthenticated = errors.New("unauthenticated")
	ErrAuthUnavailable = errors.New("authentication unavailable")
)

const (
	maxBearerBytes        = 8192
	maxIntrospectionBytes = 64 << 10
	introspectionTimeout  = 5 * time.Second
)

// FileAuthenticator reopens its Secret-backed configuration and validates every
// credential on every HTTP request. It does not cache either manual mappings
// or OAuth introspection results. Keep the mounted Secret file available and
// use a projected volume (not subPath) for Kubernetes rotation to take effect.
type FileAuthenticator struct {
	path   string
	client *http.Client
	now    func() time.Time
}

// NewFileAuthenticator checks configuration immediately so a misconfigured
// externally reachable listener never starts. Later read failures fail closed.
func NewFileAuthenticator(path string) (*FileAuthenticator, error) {
	if _, err := LoadConfig(path); err != nil {
		return nil, err
	}
	return &FileAuthenticator{
		path: path,
		client: &http.Client{
			Timeout: introspectionTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				// Never send tokens or introspection credentials to redirects.
				return http.ErrUseLastResponse
			},
		},
		now: time.Now,
	}, nil
}

// Authenticate accepts only one strict Authorization: Bearer header. Request
// bodies, cookies, tool arguments, and URL query parameters never provide an
// identity. The caller must separately verify that the mapped representative
// is active before reading or writing organization data.
func (a *FileAuthenticator) Authenticate(r *http.Request) (Principal, error) {
	if a == nil || r == nil {
		return Principal{}, ErrAuthUnavailable
	}
	cfg, err := LoadConfig(a.path)
	if err != nil {
		return Principal{}, ErrAuthUnavailable
	}
	token, ok := bearerToken(r)
	if !ok {
		return Principal{}, ErrUnauthenticated
	}
	// A fixed-size SHA-256 comparison avoids variable-length secret comparison.
	// Scan all configured keys instead of revealing which principal matched by
	// returning early. Duplicate digests were already rejected by Validate.
	want := sha256.Sum256([]byte(token))
	var principal Principal
	matched := 0
	for _, p := range cfg.Principals {
		for _, encoded := range p.BearerSHA256 {
			digest, _ := hex.DecodeString(encoded) // validated fixed length
			equal := subtle.ConstantTimeCompare(want[:], digest)
			matched |= equal
			if equal == 1 {
				principal = p.Principal
			}
		}
	}
	if matched == 1 {
		return principal, nil
	}
	if cfg.OAuth == nil {
		return Principal{}, ErrUnauthenticated
	}
	principal, err = a.introspect(r.Context(), cfg, token)
	if err != nil {
		return Principal{}, err
	}
	// An operator may revoke a mapping or switch issuers during the bounded
	// network round trip. Do not grant access against the earlier snapshot.
	latest, err := LoadConfig(a.path)
	if err != nil || !reflect.DeepEqual(cfg, latest) {
		return Principal{}, ErrAuthUnavailable
	}
	return principal, nil
}

func bearerToken(r *http.Request) (string, bool) {
	var values []string
	// Header.Get silently ignores duplicate lines. Case-fold the map keys too
	// so constructed requests and unusual reverse proxies cannot create an
	// alternate-casing authorization header alongside the canonical one.
	for name, v := range r.Header {
		if strings.EqualFold(name, "Authorization") {
			values = append(values, v...)
		}
	}
	if len(values) != 1 {
		return "", false
	}
	h := values[0]
	if len(h) <= 7 || len(h) > maxBearerBytes+7 || !strings.EqualFold(h[:7], "Bearer ") {
		return "", false
	}
	token := h[7:]
	// RFC 6750 b64token syntax. A single separating space is required; commas,
	// control characters, Unicode whitespace, and combined credentials fail.
	padding := false
	for _, b := range []byte(token) {
		if b == '=' {
			padding = true
			continue
		}
		if padding || !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.ContainsRune("-._~+/", rune(b))) {
			return "", false
		}
	}
	if token[0] == '=' {
		return "", false
	}
	return token, true
}

func (a *FileAuthenticator) introspect(ctx context.Context, cfg *Config, token string) (Principal, error) {
	o := cfg.OAuth
	form := url.Values{"token": {token}, "token_type_hint": {"access_token"}, "resource": {cfg.ResourceURL}}
	ctx, cancel := context.WithTimeout(ctx, introspectionTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.IntrospectionURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Principal{}, ErrAuthUnavailable
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// RFC 6749 section 2.3.1 requires form-encoding each credential before
	// combining them into HTTP Basic authentication.
	req.SetBasicAuth(url.QueryEscape(o.ClientID), url.QueryEscape(o.ClientSecret))
	resp, err := a.client.Do(req)
	if err != nil {
		return Principal{}, ErrAuthUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Do not surface provider bodies or errors (they may contain tokens).
		return Principal{}, ErrAuthUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxIntrospectionBytes+1))
	if err != nil || len(data) > maxIntrospectionBytes || uniqueJSONKeys(data) != nil {
		return Principal{}, ErrUnauthenticated
	}
	var claims map[string]json.RawMessage
	if err := json.Unmarshal(data, &claims); err != nil || claims == nil {
		return Principal{}, ErrUnauthenticated
	}
	// Read exact case-sensitive claim names. In particular, never trust a
	// token's own unverified JWT payload, or an issuer URL supplied by it.
	var active bool
	var issuer, subject, client, scope string
	var expiration int64
	if !claim(claims, "active", &active) || !active ||
		!claim(claims, "iss", &issuer) || issuer != o.Issuer ||
		!singleResource(claims["aud"], cfg.ResourceURL) ||
		!claim(claims, "exp", &expiration) || expiration <= a.now().Unix() ||
		!claim(claims, "sub", &subject) || !claim(claims, "client_id", &client) ||
		!claim(claims, "scope", &scope) || !hasScope(scope, o.RequiredScope) {
		return Principal{}, ErrUnauthenticated
	}
	if resource, present := claims["resource"]; present && !singleResource(resource, cfg.ResourceURL) {
		return Principal{}, ErrUnauthenticated
	}
	for _, name := range []string{"nbf", "iat"} {
		if _, present := claims[name]; present {
			var timestamp int64
			if !claim(claims, name, &timestamp) || timestamp < 0 || timestamp > a.now().Unix() {
				return Principal{}, ErrUnauthenticated
			}
		}
	}
	if _, present := claims["token_type"]; present {
		var kind string
		if !claim(claims, "token_type", &kind) || !strings.EqualFold(kind, "Bearer") {
			return Principal{}, ErrUnauthenticated
		}
	}
	// Proof-of-possession tokens cannot be safely accepted without validating
	// their binding, which this bearer-only resource server does not support.
	if _, present := claims["cnf"]; present {
		return Principal{}, ErrUnauthenticated
	}
	for _, p := range cfg.Principals {
		if p.OAuthSubject != "" && p.OAuthSubject == subject && p.OAuthClientID == client {
			return p.Principal, nil
		}
	}
	return Principal{}, ErrUnauthenticated
}

func claim(claims map[string]json.RawMessage, name string, dst any) bool {
	raw, ok := claims[name]
	return ok && string(raw) != "null" && json.Unmarshal(raw, dst) == nil
}

func singleResource(raw json.RawMessage, want string) bool {
	var single string
	if json.Unmarshal(raw, &single) == nil {
		return single == want
	}
	var many []string
	return json.Unmarshal(raw, &many) == nil && len(many) == 1 && many[0] == want
}

func hasScope(scopes, want string) bool {
	found := false
	for _, scope := range strings.Split(scopes, " ") {
		if !scopeToken(scope) {
			return false
		}
		found = found || scope == want
	}
	return found
}

// Challenge advertises only the validated, operator-configured public URL;
// untrusted Host or forwarded headers never determine the discovery address.
// On config failure it reveals no stale discovery information.
func (a *FileAuthenticator) Challenge() string {
	if a == nil {
		return "Bearer"
	}
	cfg, err := LoadConfig(a.path)
	if err != nil {
		return "Bearer"
	}
	u, _ := url.Parse(cfg.ResourceURL)
	metadataURL := "https://" + u.Host + "/.well-known/oauth-protected-resource" + strings.TrimRight(u.EscapedPath(), "/")
	challenge := "Bearer resource_metadata=" + quotedChallenge(metadataURL)
	if cfg.OAuth != nil {
		challenge += ", scope=" + quotedChallenge(cfg.OAuth.RequiredScope)
	}
	return challenge
}

func quotedChallenge(value string) string {
	// Both values were validated to exclude quotation marks and controls;
	// URL serialization percent-escapes quotes in resource paths.
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
}

// Discovery serves RFC 9728 protected-resource metadata with the SDK's CORS and
// method handling. Mount it at both the root and path-suffixed well-known
// routes. It exposes no client secret, principal mapping, or bearer digest.
func (a *FileAuthenticator) Discovery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if a == nil {
		http.Error(w, "authentication unavailable", http.StatusServiceUnavailable)
		return
	}
	cfg, err := LoadConfig(a.path)
	if err != nil {
		http.Error(w, "authentication unavailable", http.StatusServiceUnavailable)
		return
	}
	metadata := &oauthex.ProtectedResourceMetadata{
		Resource:               cfg.ResourceURL,
		ResourceName:           "Steadmesh Representative",
		BearerMethodsSupported: []string{"header"},
	}
	if cfg.OAuth != nil {
		metadata.AuthorizationServers = []string{cfg.OAuth.Issuer}
		metadata.ScopesSupported = []string{cfg.OAuth.RequiredScope}
	}
	mcpauth.ProtectedResourceMetadataHandler(metadata).ServeHTTP(w, r)
}
