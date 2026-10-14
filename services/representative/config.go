package representative

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"unicode"
)

const (
	maxConfigBytes = 1 << 20
	maxPrincipals  = 1024
	maxBearerKeys  = 16
)

// Principal is a trusted mapping from an external credential to one
// representative. None of these fields may come from MCP tool arguments.
type Principal struct {
	ID                    string `json:"id"`
	OrganizationID        string `json:"organizationID"`
	RepresentativeSeatKey string `json:"representativeSeatKey"`
	CallerID              string `json:"callerID"`
}

// PrincipalConfig allows overlapping manual keys during rotation, or an exact
// OAuth subject/client pair, or both. BearerSHA256 holds lowercase hex SHA-256
// digests, never plaintext keys. Generate keys from at least 32 random bytes:
// a digest alone cannot prove that its original key had sufficient entropy.
type PrincipalConfig struct {
	Principal
	BearerSHA256  []string `json:"bearerSHA256,omitempty"`
	OAuthSubject  string   `json:"oauthSubject,omitempty"`
	OAuthClientID string   `json:"oauthClientID,omitempty"`
}

// OAuthConfig describes an existing authorization server, not an authorization
// server implemented by this gateway. It must support RFC 7662 introspection
// with client_secret_basic and return active, iss, aud, exp, sub, client_id,
// and scope. The sole accepted audience is Config.ResourceURL.
type OAuthConfig struct {
	Issuer           string `json:"issuer"`
	IntrospectionURL string `json:"introspectionURL"`
	ClientID         string `json:"clientID"`
	ClientSecret     string `json:"clientSecret"`
	RequiredScope    string `json:"requiredScope"`
}

// Config must be mounted from an operator-managed Secret, not embedded in
// public chart values. The whole file is validated anew on every request.
type Config struct {
	ResourceURL string            `json:"resourceURL"`
	OAuth       *OAuthConfig      `json:"oauth,omitempty"`
	Principals  []PrincipalConfig `json:"principals"`
}

var ErrInvalidConfig = errors.New("invalid representative authentication configuration")

// LoadConfig opens the file each time, including following Kubernetes Secret
// volume symlink updates. Invalid, missing, or oversized files fail closed;
// the last successfully read configuration is deliberately never cached.
func LoadConfig(path string) (*Config, error) {
	if path == "" {
		return nil, ErrInvalidConfig
	}
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() > maxConfigBytes {
		return nil, ErrInvalidConfig
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrInvalidConfig
	}
	defer f.Close()
	st, err = f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, ErrInvalidConfig
	}
	data, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil || len(data) > maxConfigBytes {
		return nil, ErrInvalidConfig
	}
	if err := uniqueJSONKeys(data); err != nil {
		return nil, ErrInvalidConfig
	}
	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, ErrInvalidConfig
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate rejects ambiguous mappings and malformed policy before any
// credential is considered. Errors contain field descriptions, never values.
func (c *Config) Validate() error {
	bad := func(field string) error { return fmt.Errorf("%w: %s", ErrInvalidConfig, field) }
	if c == nil || !httpsURL(c.ResourceURL) {
		return bad("resourceURL must be an absolute HTTPS URL without credentials, query, or fragment")
	}
	resource, _ := url.Parse(c.ResourceURL)
	if resource.Path != "/mcp" || resource.EscapedPath() != "/mcp" {
		return bad("resourceURL path must be exactly /mcp")
	}
	if len(c.Principals) == 0 || len(c.Principals) > maxPrincipals {
		return bad("principals count")
	}
	if o := c.OAuth; o != nil {
		if !httpsURL(o.Issuer) || !httpsURL(o.IntrospectionURL) {
			return bad("OAuth URLs must be HTTPS without credentials, query, or fragment")
		}
		if !cleanValue(o.ClientID, 512) || !cleanValue(o.ClientSecret, 4096) || !scopeToken(o.RequiredScope) {
			return bad("OAuth client credentials or requiredScope")
		}
	}
	ids := make(map[string]bool)
	callers := make(map[string]bool)
	keys := make(map[string]bool)
	mappings := make(map[string]bool)
	for _, p := range c.Principals {
		if !cleanValue(p.ID, 256) || !cleanValue(p.OrganizationID, 256) || !cleanValue(p.RepresentativeSeatKey, 256) || !cleanValue(p.CallerID, 256) {
			return bad("principal identity fields")
		}
		// Caller IDs are audit identities, so they must not alias another
		// principal even across organizations or representative seats.
		if ids[p.ID] || callers[p.CallerID] {
			return bad("duplicate principal ID or callerID")
		}
		ids[p.ID], callers[p.CallerID] = true, true
		if len(p.BearerSHA256) > maxBearerKeys {
			return bad("too many bearer digests")
		}
		for _, key := range p.BearerSHA256 {
			digest, err := hex.DecodeString(key)
			if err != nil || len(digest) != sha256.Size || strings.ToLower(key) != key || keys[key] {
				return bad("invalid or duplicate bearer digest")
			}
			keys[key] = true
		}
		hasOAuth := p.OAuthSubject != "" || p.OAuthClientID != ""
		if hasOAuth {
			if c.OAuth == nil || !cleanValue(p.OAuthSubject, 1024) || !cleanValue(p.OAuthClientID, 512) {
				return bad("OAuth mapping requires issuer, subject, and client ID")
			}
			pair := p.OAuthSubject + "\x00" + p.OAuthClientID
			if mappings[pair] {
				return bad("duplicate OAuth subject/client mapping")
			}
			mappings[pair] = true
		}
		if len(p.BearerSHA256) == 0 && !hasOAuth {
			return bad("principal has no credentials")
		}
	}
	if c.OAuth != nil && len(mappings) == 0 {
		return bad("OAuth has no principal mappings")
	}
	return nil
}

func cleanValue(s string, limit int) bool {
	return s != "" && len(s) <= limit && !strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	})
}

// RFC 6749 scope-token permits printable ASCII except quote and backslash.
func scopeToken(s string) bool {
	if s == "" || len(s) > 256 {
		return false
	}
	for _, b := range []byte(s) {
		if b < 0x21 || b > 0x7e || b == '"' || b == '\\' {
			return false
		}
	}
	return true
}

func httpsURL(raw string) bool {
	if !cleanValue(raw, 2048) {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return false
	}
	if strings.HasSuffix(u.Host, ":") {
		return false
	}
	if p := u.Port(); p != "" {
		port, err := strconv.Atoi(p)
		if err != nil || port < 1 || port > 65535 {
			return false
		}
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

// encoding/json accepts duplicate and case-insensitively colliding keys. Both
// are forbidden, including in introspection responses, to avoid parser
// disagreement about security-critical claims. Unknown introspection claims
// remain allowed. The depth bound avoids pathological nested input.
func uniqueJSONKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 32 {
			return ErrInvalidConfig
		}
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := make(map[string]bool)
			for dec.More() {
				tok, err := dec.Token()
				if err != nil {
					return err
				}
				key, ok := tok.(string)
				if !ok {
					return ErrInvalidConfig
				}
				key = strings.ToLower(key)
				if seen[key] {
					return ErrInvalidConfig
				}
				seen[key] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for dec.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		default:
			return ErrInvalidConfig
		}
		_, err = dec.Token()
		return err
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return ErrInvalidConfig
	}
	return nil
}
