package representative

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const authTestKey = "37f4418507664a4a55b01a2496236e932a7986f9a088f49468da475c620249241"

func authTestDigest(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func authTestConfig() Config {
	return Config{
		ResourceURL: "https://representative.example.com/mcp",
		Principals: []PrincipalConfig{{
			Principal:    Principal{ID: "alice", OrganizationID: "org-one", RepresentativeSeatKey: "rep_a", CallerID: "dot:alice"},
			BearerSHA256: []string{authTestDigest(authTestKey)},
		}},
	}
}

func authTestWriteConfig(t *testing.T, path string, cfg Config) {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func authTestFile(t *testing.T, cfg Config) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway.json")
	authTestWriteConfig(t, path, cfg)
	return path
}

func authTestOAuthConfig() Config {
	cfg := authTestConfig()
	cfg.OAuth = &OAuthConfig{
		Issuer: "https://issuer.example.com/tenant", IntrospectionURL: "https://issuer.example.com/introspect",
		ClientID: "resource-client", ClientSecret: "private-introspection-secret", RequiredScope: "steadmesh:representative",
	}
	cfg.Principals[0].OAuthSubject = "alice-sub"
	cfg.Principals[0].OAuthClientID = "chatgpt-client"
	return cfg
}

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Config)
	}{
		{"resource HTTP", func(c *Config) { c.ResourceURL = "http://gateway.example/mcp" }},
		{"resource relative", func(c *Config) { c.ResourceURL = "/mcp" }},
		{"resource different path", func(c *Config) { c.ResourceURL = "https://gateway.example/other" }},
		{"resource no path", func(c *Config) { c.ResourceURL = "https://gateway.example" }},
		{"resource trailing slash", func(c *Config) { c.ResourceURL += "/" }},
		{"resource encoded path", func(c *Config) { c.ResourceURL = "https://gateway.example/%6dcp" }},
		{"resource encoded slash", func(c *Config) { c.ResourceURL = "https://gateway.example%2fmcp" }},
		{"resource no host", func(c *Config) { c.ResourceURL = "https:///mcp" }},
		{"resource userinfo", func(c *Config) { c.ResourceURL = "https://secret@example.com/mcp" }},
		{"resource query", func(c *Config) { c.ResourceURL += "?token=secret" }},
		{"resource empty query", func(c *Config) { c.ResourceURL += "?" }},
		{"resource fragment", func(c *Config) { c.ResourceURL += "#fragment" }},
		{"resource empty fragment", func(c *Config) { c.ResourceURL += "#" }},
		{"resource whitespace", func(c *Config) { c.ResourceURL += " " }},
		{"resource traversal", func(c *Config) { c.ResourceURL = "https://gateway.example/a/%2e%2e/mcp" }},
		{"resource bad port", func(c *Config) { c.ResourceURL = "https://gateway.example:70000/mcp" }},
		{"resource empty port", func(c *Config) { c.ResourceURL = "https://gateway.example:/mcp" }},
		{"no principals", func(c *Config) { c.Principals = nil }},
		{"too many principals", func(c *Config) { c.Principals = make([]PrincipalConfig, maxPrincipals+1) }},
		{"empty principal ID", func(c *Config) { c.Principals[0].ID = "" }},
		{"empty organization", func(c *Config) { c.Principals[0].OrganizationID = "" }},
		{"empty seat", func(c *Config) { c.Principals[0].RepresentativeSeatKey = "" }},
		{"empty caller", func(c *Config) { c.Principals[0].CallerID = "" }},
		{"whitespace identity", func(c *Config) { c.Principals[0].CallerID = "dot:alice\nadmin" }},
		{"Unicode whitespace identity", func(c *Config) { c.Principals[0].CallerID = "dot:alice\u00a0admin" }},
		{"no credentials", func(c *Config) { c.Principals[0].BearerSHA256 = nil }},
		{"plaintext key", func(c *Config) { c.Principals[0].BearerSHA256 = []string{"not-a-digest"} }},
		{"short digest", func(c *Config) { c.Principals[0].BearerSHA256 = []string{strings.Repeat("0", 62)} }},
		{"long digest", func(c *Config) { c.Principals[0].BearerSHA256 = []string{strings.Repeat("0", 66)} }},
		{"uppercase digest", func(c *Config) { c.Principals[0].BearerSHA256 = []string{strings.Repeat("A", 64)} }},
		{"nonhex digest", func(c *Config) { c.Principals[0].BearerSHA256 = []string{strings.Repeat("x", 64)} }},
		{"duplicate key", func(c *Config) {
			c.Principals[0].BearerSHA256 = append(c.Principals[0].BearerSHA256, c.Principals[0].BearerSHA256[0])
		}},
		{"too many keys", func(c *Config) { c.Principals[0].BearerSHA256 = make([]string, maxBearerKeys+1) }},
		{"duplicate principal", func(c *Config) { c.Principals = append(c.Principals, c.Principals[0]) }},
		{"orphan subject", func(c *Config) { c.Principals[0].OAuthSubject = "alice" }},
		{"orphan client", func(c *Config) { c.Principals[0].OAuthClientID = "client" }},
		{"mapping without issuer", func(c *Config) { c.Principals[0].OAuthSubject, c.Principals[0].OAuthClientID = "alice", "client" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := authTestConfig()
			tt.edit(&cfg)
			if _, err := LoadConfig(authTestFile(t, cfg)); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("LoadConfig() = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestConfigOAuthValidation(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Config)
	}{
		{"HTTP issuer", func(c *Config) { c.OAuth.Issuer = "http://issuer.example.com" }},
		{"HTTP introspection", func(c *Config) { c.OAuth.IntrospectionURL = "http://issuer.example.com/introspect" }},
		{"credential in endpoint", func(c *Config) { c.OAuth.IntrospectionURL = "https://user:secret@issuer.example.com/introspect" }},
		{"missing client ID", func(c *Config) { c.OAuth.ClientID = "" }},
		{"missing client secret", func(c *Config) { c.OAuth.ClientSecret = "" }},
		{"credential whitespace", func(c *Config) { c.OAuth.ClientSecret = "secret\n" }},
		{"missing scope", func(c *Config) { c.OAuth.RequiredScope = "" }},
		{"multiple scopes", func(c *Config) { c.OAuth.RequiredScope = "read write" }},
		{"quoted scope", func(c *Config) { c.OAuth.RequiredScope = `read"write` }},
		{"backslash scope", func(c *Config) { c.OAuth.RequiredScope = `read\write` }},
		{"no mappings", func(c *Config) { c.Principals[0].OAuthSubject, c.Principals[0].OAuthClientID = "", "" }},
		{"partial mapping", func(c *Config) { c.Principals[0].OAuthClientID = "" }},
		{"mapping whitespace", func(c *Config) { c.Principals[0].OAuthSubject = " alice" }},
		{"duplicate mapping", func(c *Config) {
			p := c.Principals[0]
			p.ID, p.CallerID, p.BearerSHA256 = "other", "other", nil
			c.Principals = append(c.Principals, p)
		}},
		{"duplicate caller", func(c *Config) {
			p := c.Principals[0]
			p.ID, p.OAuthSubject, p.BearerSHA256 = "other", "other", nil
			c.Principals = append(c.Principals, p)
		}},
		{"cross-principal duplicate digest", func(c *Config) {
			p := c.Principals[0]
			p.ID, p.CallerID, p.OAuthSubject = "other", "other", "other"
			c.Principals = append(c.Principals, p)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := authTestOAuthConfig()
			tt.edit(&cfg)
			if err := cfg.Validate(); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("Validate() = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestLoadConfigRejectsMalformedJSON(t *testing.T) {
	cfg := authTestConfig()
	data, _ := json.Marshal(cfg)
	for name, raw := range map[string]string{
		"unknown field":     strings.Replace(string(data), `"resourceURL":`, `"unknown":"private-token","resourceURL":`, 1),
		"duplicate field":   strings.Replace(string(data), `"resourceURL":`, `"resourceURL":"https://evil.example/mcp","resourceURL":`, 1),
		"case collision":    strings.Replace(string(data), `"resourceURL":`, `"ResourceURL":"https://evil.example/mcp","resourceURL":`, 1),
		"nested duplicate":  strings.Replace(string(data), `"callerID":`, `"callerID":"evil","callerID":`, 1),
		"escaped duplicate": strings.Replace(string(data), `"callerID":`, `"caller\u0049D":"evil","callerID":`, 1),
		"trailing object":   string(data) + `{}`,
		"invalid syntax":    `{"secret":"private-token"`,
		"null":              `null`,
		"array":             `[]`,
		"oversized":         strings.Repeat(" ", maxConfigBytes+1),
		"deeply nested":     strings.Repeat("[", 40) + "0" + strings.Repeat("]", 40),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "gateway.json")
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadConfig(path)
			if !errors.Is(err, ErrInvalidConfig) || strings.Contains(err.Error(), "private-token") {
				t.Fatalf("unexpected config error: %v", err)
			}
		})
	}
}

func TestLoadConfigFileBoundaries(t *testing.T) {
	for _, path := range []string{"", t.TempDir(), filepath.Join(t.TempDir(), "missing.json")} {
		if _, err := LoadConfig(path); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("LoadConfig(%q) = %v", path, err)
		}
	}
	cfg := authTestOAuthConfig()
	if _, err := LoadConfig(authTestFile(t, cfg)); err != nil {
		t.Fatal(err)
	}
	if err := (*Config)(nil).Validate(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil config: %v", err)
	}
}
