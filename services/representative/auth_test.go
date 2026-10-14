package representative

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func authTestRequest(token string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "https://representative.example.com/mcp", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

func authTestAuthenticator(t *testing.T, cfg Config) *FileAuthenticator {
	t.Helper()
	a, err := NewFileAuthenticator(authTestFile(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestManualBearerPrincipalAndRotation(t *testing.T) {
	cfg := authTestConfig()
	otherKey := strings.Repeat("c", 64)
	other := PrincipalConfig{
		Principal:    Principal{ID: "bob", OrganizationID: "org-two", RepresentativeSeatKey: "rep_b", CallerID: "dot:bob"},
		BearerSHA256: []string{authTestDigest(otherKey)},
	}
	cfg.Principals = append(cfg.Principals, other)
	a := authTestAuthenticator(t, cfg)
	for token, want := range map[string]Principal{authTestKey: cfg.Principals[0].Principal, otherKey: other.Principal} {
		r := authTestRequest(token)
		// None of these untrusted selectors may change the mapped principal.
		r.URL.RawQuery = "organizationID=attacker&representativeSeatKey=admin&callerID=bob"
		r.Header.Set("X-Organization-ID", "attacker")
		r.Header.Set("X-Representative-Seat", "admin")
		got, err := a.Authenticate(r)
		if err != nil || got != want {
			t.Fatalf("Authenticate() = %#v, %v; want %#v", got, err, want)
		}
	}
	rotated := strings.Repeat("d", 64)
	cfg.Principals[0].BearerSHA256 = append(cfg.Principals[0].BearerSHA256, authTestDigest(rotated))
	authTestWriteConfig(t, a.path, cfg)
	for _, token := range []string{authTestKey, rotated} {
		if _, err := a.Authenticate(authTestRequest(token)); err != nil {
			t.Fatalf("overlapping rotation: %v", err)
		}
	}
	cfg.Principals[0].BearerSHA256 = []string{authTestDigest(rotated)}
	authTestWriteConfig(t, a.path, cfg)
	if _, err := a.Authenticate(authTestRequest(authTestKey)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked key = %v", err)
	}
	if got, err := a.Authenticate(authTestRequest(rotated)); err != nil || got.ID != "alice" {
		t.Fatalf("rotated key = %#v, %v", got, err)
	}
	cfg.Principals = cfg.Principals[1:]
	authTestWriteConfig(t, a.path, cfg)
	if _, err := a.Authenticate(authTestRequest(rotated)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("removed principal = %v", err)
	}
}

func TestManualBearerMalformedHeaders(t *testing.T) {
	a := authTestAuthenticator(t, authTestConfig())
	for name, values := range map[string][]string{
		"missing":             nil,
		"empty":               {""},
		"no token":            {"Bearer "},
		"wrong scheme":        {"Basic " + authTestKey},
		"missing separator":   {"Bearer" + authTestKey},
		"leading space":       {" Bearer " + authTestKey},
		"two spaces":          {"Bearer  " + authTestKey},
		"tab separator":       {"Bearer\t" + authTestKey},
		"trailing space":      {"Bearer " + authTestKey + " "},
		"newline":             {"Bearer " + authTestKey + "\n"},
		"nul":                 {"Bearer " + authTestKey + "\x00"},
		"Unicode whitespace":  {"Bearer " + authTestKey + "\u00a0"},
		"comma combined":      {"Bearer " + authTestKey + ", Bearer " + authTestKey},
		"duplicate identical": {"Bearer " + authTestKey, "Bearer " + authTestKey},
		"duplicate different": {"Bearer " + authTestKey, "Bearer other"},
		"duplicate empty":     {"Bearer " + authTestKey, ""},
		"invalid punctuation": {"Bearer key:secret"},
		"padding only":        {"Bearer ="},
		"embedded padding":    {"Bearer key=secret"},
		"oversized":           {"Bearer " + strings.Repeat("x", maxBearerBytes+1)},
		"unknown short token": {"Bearer x"},
		"unknown same length": {"Bearer " + strings.Repeat("0", len(authTestKey))},
	} {
		t.Run(name, func(t *testing.T) {
			r := authTestRequest(authTestKey)
			r.Header["Authorization"] = values
			if got, err := a.Authenticate(r); !errors.Is(err, ErrUnauthenticated) || got != (Principal{}) {
				t.Fatalf("Authenticate() = %#v, %v", got, err)
			}
		})
	}
	for _, scheme := range []string{"Bearer", "bearer", "BEARER", "BeArEr"} {
		r := authTestRequest(authTestKey)
		r.Header.Set("Authorization", scheme+" "+authTestKey)
		if _, err := a.Authenticate(r); err != nil {
			t.Fatalf("case-insensitive scheme: %v", err)
		}
	}
	r := authTestRequest(authTestKey)
	r.Header["authorization"] = []string{"Bearer " + authTestKey}
	if _, err := a.Authenticate(r); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("differently cased duplicate header: %v", err)
	}
	for _, query := range []string{"access_token=", "api_key=", "token="} {
		r := httptest.NewRequest(http.MethodGet, "/mcp?"+query+authTestKey, nil)
		if _, err := a.Authenticate(r); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("query credential authenticated: %v", err)
		}
	}
	r = httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"token":"`+authTestKey+`"}`))
	r.AddCookie(&http.Cookie{Name: "token", Value: authTestKey})
	if _, err := a.Authenticate(r); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("body/cookie credential authenticated: %v", err)
	}
}

func TestConfigReloadFailsClosed(t *testing.T) {
	cfg := authTestConfig()
	a := authTestAuthenticator(t, cfg)
	if _, err := a.Authenticate(authTestRequest(authTestKey)); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(){
		"corrupt":         func() { _ = os.WriteFile(a.path, []byte(`{"private-secret":`), 0600) },
		"invalid mapping": func() { broken := cfg; broken.Principals = nil; authTestWriteConfig(t, a.path, broken) },
		"delete":          func() { _ = os.Remove(a.path) },
		"directory":       func() { _ = os.Remove(a.path); _ = os.Mkdir(a.path, 0700) },
	} {
		t.Run(name, func(t *testing.T) {
			_ = os.RemoveAll(a.path)
			authTestWriteConfig(t, a.path, cfg)
			change()
			if got, err := a.Authenticate(authTestRequest(authTestKey)); !errors.Is(err, ErrAuthUnavailable) || got != (Principal{}) {
				t.Fatalf("unavailable config: %#v, %v", got, err)
			}
			if a.Challenge() != "Bearer" {
				t.Fatal("invalid config leaked stale challenge")
			}
			w := httptest.NewRecorder()
			a.Discovery(w, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource", nil))
			if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "private-secret") {
				t.Fatalf("invalid config discovery: %d %s", w.Code, w.Body.String())
			}
		})
	}
	if _, err := NewFileAuthenticator(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("startup accepted missing config: %v", err)
	}
}

func TestProjectedSecretSymlinkRotation(t *testing.T) {
	cfg := authTestConfig()
	dir := t.TempDir()
	v1, v2, link := filepath.Join(dir, "v1"), filepath.Join(dir, "v2"), filepath.Join(dir, "gateway.json")
	authTestWriteConfig(t, v1, cfg)
	if err := os.Symlink(v1, link); err != nil {
		t.Fatal(err)
	}
	a, err := NewFileAuthenticator(link)
	if err != nil {
		t.Fatal(err)
	}
	newKey := strings.Repeat("b", 64)
	cfg.Principals[0].BearerSHA256 = []string{authTestDigest(newKey)}
	authTestWriteConfig(t, v2, cfg)
	if err := os.Symlink(v2, link+".new"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(link+".new", link); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate(authTestRequest(authTestKey)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("old projected Secret still accepted: %v", err)
	}
	if _, err := a.Authenticate(authTestRequest(newKey)); err != nil {
		t.Fatalf("new projected Secret rejected: %v", err)
	}
}

func authTestClaims() map[string]any {
	return map[string]any{
		"active": true, "iss": "https://issuer.example.com/tenant", "aud": "https://representative.example.com/mcp",
		"exp": time.Now().Add(time.Hour).Unix(), "sub": "alice-sub", "client_id": "chatgpt-client",
		"scope": "steadmesh:representative", "token_type": "Bearer",
	}
}

func authTestIntrospectionServer(t *testing.T, handler http.HandlerFunc) (*FileAuthenticator, *httptest.Server) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	cfg := authTestOAuthConfig()
	cfg.OAuth.IntrospectionURL = server.URL + "/introspect"
	a := authTestAuthenticator(t, cfg)
	a.client.Transport = server.Client().Transport
	return a, server
}

func TestOAuthIntrospectionClaims(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(map[string]any)
		valid bool
	}{
		{"valid", func(c map[string]any) {}, true},
		{"singleton audience", func(c map[string]any) { c["aud"] = []string{"https://representative.example.com/mcp"} }, true},
		{"extra scopes", func(c map[string]any) { c["scope"] = "read steadmesh:representative write" }, true},
		{"matching resource", func(c map[string]any) { c["resource"] = "https://representative.example.com/mcp" }, true},
		{"unknown provider claims", func(c map[string]any) { c["extension"] = map[string]any{"value": "ignored"} }, true},
		{"inactive", func(c map[string]any) { c["active"] = false }, false},
		{"missing active", func(c map[string]any) { delete(c, "active") }, false},
		{"null active", func(c map[string]any) { c["active"] = nil }, false},
		{"string active", func(c map[string]any) { c["active"] = "true" }, false},
		{"wrong issuer", func(c map[string]any) { c["iss"] = "https://evil.example.com" }, false},
		{"issuer prefix", func(c map[string]any) { c["iss"] = "https://issuer.example.com/tenant/attacker" }, false},
		{"issuer trailing slash", func(c map[string]any) { c["iss"] = "https://issuer.example.com/tenant/" }, false},
		{"missing issuer", func(c map[string]any) { delete(c, "iss") }, false},
		{"wrong issuer case", func(c map[string]any) { c["ISS"] = c["iss"]; delete(c, "iss") }, false},
		{"wrong audience", func(c map[string]any) { c["aud"] = "https://internal.example.com/api" }, false},
		{"audience prefix", func(c map[string]any) { c["aud"] = "https://representative.example.com/mcp/other" }, false},
		{"multiple audience", func(c map[string]any) { c["aud"] = []string{"https://representative.example.com/mcp", "other"} }, false},
		{"audience empty", func(c map[string]any) { c["aud"] = []string{} }, false},
		{"missing audience", func(c map[string]any) { delete(c, "aud") }, false},
		{"wrong resource", func(c map[string]any) { c["resource"] = "https://evil.example.com/mcp" }, false},
		{"null resource", func(c map[string]any) { c["resource"] = nil }, false},
		{"expired", func(c map[string]any) { c["exp"] = time.Now().Add(-time.Second).Unix() }, false},
		{"expires now", func(c map[string]any) { c["exp"] = time.Now().Unix() }, false},
		{"missing expiration", func(c map[string]any) { delete(c, "exp") }, false},
		{"null expiration", func(c map[string]any) { c["exp"] = nil }, false},
		{"string expiration", func(c map[string]any) { c["exp"] = "9999999999" }, false},
		{"fraction expiration", func(c map[string]any) { c["exp"] = 9999999999.5 }, false},
		{"overflow expiration", func(c map[string]any) { c["exp"] = json.Number("999999999999999999999999999") }, false},
		{"wrong subject", func(c map[string]any) { c["sub"] = "bob-sub" }, false},
		{"missing subject", func(c map[string]any) { delete(c, "sub") }, false},
		{"wrong client", func(c map[string]any) { c["client_id"] = "untrusted-client" }, false},
		{"missing client", func(c map[string]any) { delete(c, "client_id") }, false},
		{"missing scope", func(c map[string]any) { delete(c, "scope") }, false},
		{"wrong scope", func(c map[string]any) { c["scope"] = "read" }, false},
		{"scope prefix", func(c map[string]any) { c["scope"] = "steadmesh:representative:admin" }, false},
		{"tab scope", func(c map[string]any) { c["scope"] = "read\tsteadmesh:representative" }, false},
		{"future not before", func(c map[string]any) { c["nbf"] = time.Now().Add(time.Hour).Unix() }, false},
		{"future issued", func(c map[string]any) { c["iat"] = time.Now().Add(time.Hour).Unix() }, false},
		{"null not before", func(c map[string]any) { c["nbf"] = nil }, false},
		{"wrong token type", func(c map[string]any) { c["token_type"] = "refresh_token" }, false},
		{"proof binding", func(c map[string]any) { c["cnf"] = map[string]string{"jkt": "thumbprint"} }, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			claims := authTestClaims()
			tt.edit(claims)
			a, _ := authTestIntrospectionServer(t, func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(claims)
			})
			got, err := a.Authenticate(authTestRequest("opaque-oauth-access-token"))
			if tt.valid {
				if err != nil || got != authTestConfig().Principals[0].Principal {
					t.Fatalf("valid claims: %#v, %v", got, err)
				}
			} else if !errors.Is(err, ErrUnauthenticated) || got != (Principal{}) {
				t.Fatalf("invalid claims authorized: %#v, %v", got, err)
			}
		})
	}
}

func TestOAuthIntrospectionRequestAndRevocation(t *testing.T) {
	var calls atomic.Int32
	var active atomic.Bool
	active.Store(true)
	a, _ := authTestIntrospectionServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		id, secret, ok := r.BasicAuth()
		if !ok || id != "resource-client" || secret != "private-introspection-secret" || r.Method != http.MethodPost || r.URL.RawQuery != "" {
			t.Error("incorrect introspection request authorization or method")
		}
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.Header.Get("Accept") != "application/json" {
			t.Error("incorrect introspection request content headers")
		}
		if err := r.ParseForm(); err != nil || r.Form.Get("token") != "opaque-token" || r.Form.Get("token_type_hint") != "access_token" || r.Form.Get("resource") != authTestConfig().ResourceURL {
			t.Error("incorrect introspection form")
		}
		claims := authTestClaims()
		claims["active"] = active.Load()
		_ = json.NewEncoder(w).Encode(claims)
	})
	for range 2 {
		if _, err := a.Authenticate(authTestRequest("opaque-token")); err != nil {
			t.Fatal(err)
		}
	}
	active.Store(false)
	if _, err := a.Authenticate(authTestRequest("opaque-token")); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked OAuth token: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("introspection responses cached: %d calls", calls.Load())
	}
	if _, err := a.Authenticate(authTestRequest(authTestKey)); err != nil || calls.Load() != 3 {
		t.Fatalf("manual credential should not require introspection: %v", err)
	}
}

func TestOAuthExactSubjectClientPair(t *testing.T) {
	claims := authTestClaims()
	a, _ := authTestIntrospectionServer(t, func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(claims) })
	cfg, err := LoadConfig(a.path)
	if err != nil {
		t.Fatal(err)
	}
	other := PrincipalConfig{Principal: Principal{ID: "bob", OrganizationID: "org-two", RepresentativeSeatKey: "rep_b", CallerID: "dot:bob"}, OAuthSubject: "bob-sub", OAuthClientID: "dot-client"}
	cfg.Principals = append(cfg.Principals, other)
	authTestWriteConfig(t, a.path, *cfg)
	for _, pair := range [][2]string{{"alice-sub", "dot-client"}, {"bob-sub", "chatgpt-client"}} {
		claims["sub"], claims["client_id"] = pair[0], pair[1]
		if _, err := a.Authenticate(authTestRequest("token")); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("cross-principal pair authenticated: %v", err)
		}
	}
	claims["sub"], claims["client_id"] = "bob-sub", "dot-client"
	if got, err := a.Authenticate(authTestRequest("token")); err != nil || got != other.Principal {
		t.Fatalf("exact second-principal pair: %#v, %v", got, err)
	}
}

func TestOAuthConfigRevokedDuringIntrospection(t *testing.T) {
	var a *FileAuthenticator
	a, _ = authTestIntrospectionServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = os.Remove(a.path)
		_ = json.NewEncoder(w).Encode(authTestClaims())
	})
	if got, err := a.Authenticate(authTestRequest("token")); !errors.Is(err, ErrAuthUnavailable) || got != (Principal{}) {
		t.Fatalf("mapping revoked during introspection: %#v, %v", got, err)
	}
}

func TestOAuthResponseBoundariesAndRedaction(t *testing.T) {
	valid, _ := json.Marshal(authTestClaims())
	for name, body := range map[string]string{
		"duplicate active":      strings.Replace(string(valid), `"active":true`, `"active":false,"active":true`, 1),
		"case-colliding active": strings.Replace(string(valid), `"active":true`, `"ACTIVE":false,"active":true`, 1),
		"duplicate issuer":      strings.Replace(string(valid), `"iss":`, `"iss":"https://evil.example","iss":`, 1),
		"trailing JSON":         string(valid) + `{}`,
		"oversized":             strings.Repeat(" ", maxIntrospectionBytes+1),
		"invalid JSON":          `secret-token-and-client-secret`,
		"null":                  `null`,
		"array":                 `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			a, _ := authTestIntrospectionServer(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) })
			got, err := a.Authenticate(authTestRequest("opaque-private-token"))
			if !errors.Is(err, ErrUnauthenticated) || got != (Principal{}) || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "opaque-private-token") {
				t.Fatalf("malformed response: %#v, %v", got, err)
			}
		})
	}
	a, server := authTestIntrospectionServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "private-client-secret private-token", http.StatusUnauthorized)
	})
	if _, err := a.Authenticate(authTestRequest("private-token")); !errors.Is(err, ErrAuthUnavailable) || strings.Contains(err.Error(), "private") {
		t.Fatalf("provider error leaked: %v", err)
	}
	server.Close()
	if _, err := a.Authenticate(authTestRequest("private-token")); !errors.Is(err, ErrAuthUnavailable) {
		t.Fatalf("unavailable issuer: %v", err)
	}
}

func TestOAuthNoRedirectAndCanceledRequests(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalls.Add(1)
		_ = json.NewEncoder(w).Encode(authTestClaims())
	}))
	defer target.Close()
	a, _ := authTestIntrospectionServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	})
	if _, err := a.Authenticate(authTestRequest("secret-token")); !errors.Is(err, ErrAuthUnavailable) || targetCalls.Load() != 0 {
		t.Fatalf("redirect followed: calls=%d err=%v", targetCalls.Load(), err)
	}
	if a.client.Timeout != introspectionTimeout {
		t.Fatal("introspection must have a bounded timeout")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Authenticate(authTestRequest("secret-token").WithContext(ctx)); !errors.Is(err, ErrAuthUnavailable) {
		t.Fatalf("canceled request: %v", err)
	}
}

func TestOAuthClientSecretBasicEncoding(t *testing.T) {
	a, _ := authTestIntrospectionServer(t, func(w http.ResponseWriter, r *http.Request) {
		id, secret, ok := r.BasicAuth()
		if !ok || id != url.QueryEscape("resource:client+id") || secret != url.QueryEscape("secret:with+symbols/") {
			t.Error("client credentials were not form-encoded before Basic encoding")
		}
		_ = json.NewEncoder(w).Encode(authTestClaims())
	})
	cfg, _ := LoadConfig(a.path)
	cfg.OAuth.ClientID, cfg.OAuth.ClientSecret = "resource:client+id", "secret:with+symbols/"
	authTestWriteConfig(t, a.path, *cfg)
	if _, err := a.Authenticate(authTestRequest("token")); err != nil {
		t.Fatal(err)
	}
}

func TestProtectedResourceDiscovery(t *testing.T) {
	cfg := authTestOAuthConfig()
	a := authTestAuthenticator(t, cfg)
	wantChallenge := `Bearer resource_metadata="https://representative.example.com/.well-known/oauth-protected-resource/mcp", scope="steadmesh:representative"`
	if got := a.Challenge(); got != wantChallenge {
		t.Fatalf("challenge = %q, want %q", got, wantChallenge)
	}
	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Host = "attacker.example"
		r.Header.Set("X-Forwarded-Host", "attacker.example")
		a.Discovery(w, r)
		if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("metadata status/headers = %d %v", w.Code, w.Header())
		}
		var metadata map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &metadata); err != nil {
			t.Fatal(err)
		}
		if metadata["resource"] != cfg.ResourceURL || !reflect.DeepEqual(metadata["authorization_servers"], []any{cfg.OAuth.Issuer}) || !reflect.DeepEqual(metadata["bearer_methods_supported"], []any{"header"}) {
			t.Fatalf("wrong metadata: %#v", metadata)
		}
		for _, private := range []string{cfg.OAuth.ClientSecret, cfg.OAuth.ClientID, cfg.Principals[0].CallerID, cfg.Principals[0].BearerSHA256[0], "attacker.example"} {
			if strings.Contains(w.Body.String(), private) {
				t.Fatal("metadata leaked private configuration or untrusted Host")
			}
		}
	}
	for method, status := range map[string]int{http.MethodOptions: http.StatusNoContent, http.MethodPost: http.StatusMethodNotAllowed} {
		w := httptest.NewRecorder()
		a.Discovery(w, httptest.NewRequest(method, "/.well-known/oauth-protected-resource", nil))
		if w.Code != status {
			t.Fatalf("method %s = %d", method, w.Code)
		}
	}
	cfg.OAuth = nil
	cfg.Principals[0].OAuthSubject, cfg.Principals[0].OAuthClientID = "", ""
	authTestWriteConfig(t, a.path, cfg)
	w := httptest.NewRecorder()
	a.Discovery(w, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource", nil))
	if strings.Contains(w.Body.String(), "authorization_servers") || strings.Contains(a.Challenge(), "scope=") {
		t.Fatal("manual-only configuration advertised stale OAuth settings")
	}
}
