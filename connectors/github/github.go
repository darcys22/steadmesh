// Package github is the GitHub connector. It authenticates as a GitHub App
// (secret keys app_id, installation_id, private_key) or with a fine-grained
// personal access token (secret key token).
//
// It serves two kinds of access (docs/sandbox.html):
//   - platform delivery: repository operations (repo.read, pull_request.*,
//     issue.*) through the gateway and its ledger; the credential never
//     leaves the platform. Created pull requests and comments carry a
//     steadmesh-op marker so an ambiguous outcome is resolved by read-back;
//   - sandbox delivery: IssueCredential returns a credential for git and gh
//     in the sandbox. With an App it is an installation token limited to the
//     granted repositories and permissions, valid for an hour, which
//     RevokeCredential invalidates at GitHub. A PAT is returned as is: only
//     as narrow as the token, and Steadmesh cannot invalidate it.
package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/connectors/internal/httpx"
)

// DefaultEndpoint is the GitHub REST API.
const DefaultEndpoint = "https://api.github.com"

// MarkerPrefix marks records created by an operation.
const MarkerPrefix = "steadmesh-op:"

// Adapter implements connectors.Tracker and connectors.CredentialIssuer.
type Adapter struct {
	base   string
	client *http.Client
	pat    string
	appID  string
	instID string
	key    *rsa.PrivateKey
	now    func() time.Time

	mu      sync.Mutex
	instTok string
	instExp time.Time
}

var (
	_ connectors.Tracker          = (*Adapter)(nil)
	_ connectors.CredentialIssuer = (*Adapter)(nil)
)

// New builds the adapter from cfg.Secret.
func New(cfg connectors.Config) (connectors.Tracker, error) { return newAdapter(cfg) }

func newAdapter(cfg connectors.Config) (*Adapter, error) {
	a := &Adapter{base: strings.TrimSuffix(cfg.Endpoint, "/"), client: httpx.Client(cfg.HTTP), now: time.Now}
	if a.base == "" {
		a.base = DefaultEndpoint
	}
	s := cfg.Secret
	switch {
	case s["app_id"] != "" || s["private_key"] != "":
		if s["app_id"] == "" || s["installation_id"] == "" || s["private_key"] == "" {
			return nil, fmt.Errorf("%w: a GitHub App secret needs app_id, installation_id and private_key", connectors.ErrUnauthorized)
		}
		key, err := parseKey(s["private_key"])
		if err != nil {
			return nil, fmt.Errorf("%w: private_key: %v", connectors.ErrUnauthorized, err)
		}
		a.appID, a.instID, a.key = s["app_id"], s["installation_id"], key
	case s["token"] != "":
		a.pat = s["token"]
	default:
		return nil, fmt.Errorf("%w: github secret must contain token, or app_id, installation_id and private_key", connectors.ErrUnauthorized)
	}
	return a, nil
}

func parseKey(p string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(p))
	if block == nil {
		return nil, errors.New("not PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an RSA key")
	}
	return rk, nil
}

// IsApp reports whether the adapter authenticates as a GitHub App.
func (a *Adapter) IsApp() bool { return a.key != nil }

// appJWT is the App's own credential, used to mint installation tokens.
func (a *Adapter) appJWT() (string, error) {
	now := a.now()
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": a.appID})
	msg := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(msg))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return msg + "." + enc.EncodeToString(sig), nil
}

// do sends a request with auth ("" uses the platform's own credential) and
// decodes a JSON response into out. It returns the classified error.
func (a *Adapter) do(ctx context.Context, method, path, auth string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, rd)
	if err != nil {
		return fmt.Errorf("%w: %v", connectors.ErrPermanent, err)
	}
	if auth == "" {
		if auth, err = a.platformToken(ctx); err != nil {
			return err
		}
	}
	req.Header.Set("Authorization", "Bearer "+auth)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return httpx.ClassifyTransport(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		detail := method + " " + path + ": " + strings.TrimSpace(string(b))
		if len(detail) > 400 {
			detail = detail[:400]
		}
		return httpx.ClassifyStatus(resp.StatusCode, detail)
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("%w: decode %s: %v", connectors.ErrPermanent, path, err)
		}
	}
	return nil
}

// platformToken is the credential for the platform's own calls: the PAT, or
// an installation token cached until shortly before it expires.
func (a *Adapter) platformToken(ctx context.Context) (string, error) {
	if !a.IsApp() {
		return a.pat, nil
	}
	a.mu.Lock()
	tok, exp := a.instTok, a.instExp
	a.mu.Unlock()
	if tok != "" && a.now().Before(exp.Add(-5*time.Minute)) {
		return tok, nil
	}
	c, err := a.mint(ctx, nil)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	a.instTok, a.instExp = c.Token, c.ExpiresAt
	a.mu.Unlock()
	return c.Token, nil
}

// mint creates an installation token, scoped when body is set.
func (a *Adapter) mint(ctx context.Context, body map[string]any) (connectors.Credential, error) {
	jwt, err := a.appJWT()
	if err != nil {
		return connectors.Credential{}, fmt.Errorf("%w: sign app token: %v", connectors.ErrPermanent, err)
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if body == nil {
		body = map[string]any{}
	}
	if err := a.do(ctx, http.MethodPost, "/app/installations/"+url.PathEscape(a.instID)+"/access_tokens", jwt, body, &out); err != nil {
		return connectors.Credential{}, err
	}
	return connectors.Credential{Username: "x-access-token", Token: out.Token, ExpiresAt: out.ExpiresAt, Revocable: true}, nil
}

// Verify checks the credential: GET /user for a PAT; for an App, GET /app
// and a minted installation token.
func (a *Adapter) Verify(ctx context.Context) error {
	if !a.IsApp() {
		err := a.do(ctx, http.MethodGet, "/user", a.pat, nil, nil)
		return retryableProbe(err)
	}
	jwt, err := a.appJWT()
	if err != nil {
		return fmt.Errorf("%w: %v", connectors.ErrPermanent, err)
	}
	if err := a.do(ctx, http.MethodGet, "/app", jwt, nil, nil); err != nil {
		return retryableProbe(err)
	}
	_, err = a.platformToken(ctx)
	return retryableProbe(err)
}

// retryableProbe treats an ambiguous probe outcome as retryable: Verify has
// no side effects.
func retryableProbe(err error) error {
	if errors.Is(err, connectors.ErrAmbiguous) {
		return fmt.Errorf("%w: %v", connectors.ErrRetryable, err)
	}
	return err
}

// IssueCredential returns a credential for the sandbox. With an App it is an
// installation token limited to the repositories and permissions; with a PAT
// it is the PAT.
func (a *Adapter) IssueCredential(ctx context.Context, scope connectors.CredentialScope) (connectors.Credential, error) {
	if !a.IsApp() {
		return connectors.Credential{Username: "x-access-token", Token: a.pat}, nil
	}
	var names []string
	for _, r := range scope.Repos {
		_, name, _ := strings.Cut(r, "/")
		names = append(names, name)
	}
	perms := map[string]string{}
	for k, v := range scope.Permissions {
		perms[k] = v
	}
	if _, ok := perms["metadata"]; !ok {
		perms["metadata"] = "read"
	}
	return a.mint(ctx, map[string]any{"repositories": names, "permissions": perms})
}

// RevokeCredential invalidates an installation token at GitHub. A PAT
// cannot be invalidated by Steadmesh.
func (a *Adapter) RevokeCredential(ctx context.Context, token string) error {
	if !a.IsApp() {
		return fmt.Errorf("%w: a personal access token can only be revoked at GitHub", connectors.ErrPermanent)
	}
	err := a.do(ctx, http.MethodDelete, "/installation/token", token, nil, nil)
	if errors.Is(err, connectors.ErrUnauthorized) {
		return nil // already expired or revoked
	}
	return err
}

// ---- operations ---------------------------------------------------------------

type params struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Head   string `json:"head"`
	Base   string `json:"base"`
	State  string `json:"state"`
}

func (a *Adapter) ReadOnly(op string) bool {
	switch op {
	case "repo.read", "pull_request.read", "issue.read":
		return true
	}
	return false
}

func marker(id string) string { return "<!-- " + MarkerPrefix + id + " -->" }

func repoPath(repo string) (string, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("%w: repo must be owner/name", connectors.ErrPermanent)
	}
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name), nil
}

func (a *Adapter) Invoke(ctx context.Context, op string, raw json.RawMessage, operationID string) (connectors.Result, error) {
	var p params
	if err := json.Unmarshal(raw, &p); err != nil {
		return connectors.Result{}, fmt.Errorf("%w: params: %v", connectors.ErrPermanent, err)
	}
	rp, err := repoPath(p.Repo)
	if err != nil {
		return connectors.Result{}, err
	}
	var out map[string]any
	switch op {
	case "repo.read":
		err = a.do(ctx, http.MethodGet, rp, "", nil, &out)
		return result(out, "full_name", err, "full_name", "default_branch", "private", "html_url", "description")
	case "pull_request.read":
		if p.Number > 0 {
			err = a.do(ctx, http.MethodGet, rp+"/pulls/"+strconv.Itoa(p.Number), "", nil, &out)
			return result(out, "number", err, "number", "title", "state", "html_url", "head", "base", "body", "merged")
		}
		var list []map[string]any
		q := url.Values{"state": {firstOr(p.State, "open")}}
		err = a.do(ctx, http.MethodGet, rp+"/pulls?"+q.Encode(), "", nil, &list)
		return listResult(list, err, "number", "title", "state", "html_url")
	case "pull_request.create":
		if p.Title == "" || p.Head == "" || p.Base == "" {
			return connectors.Result{}, fmt.Errorf("%w: pull_request.create needs title, head and base", connectors.ErrPermanent)
		}
		body := strings.TrimSpace(p.Body+"\n\n") + "\n\n" + marker(operationID)
		err = a.do(ctx, http.MethodPost, rp+"/pulls", "", map[string]any{"title": p.Title, "head": p.Head, "base": p.Base, "body": strings.TrimSpace(body)}, &out)
		return result(out, "number", err, "number", "html_url", "state")
	case "issue.read":
		if p.Number <= 0 {
			return connectors.Result{}, fmt.Errorf("%w: issue.read needs number", connectors.ErrPermanent)
		}
		err = a.do(ctx, http.MethodGet, rp+"/issues/"+strconv.Itoa(p.Number), "", nil, &out)
		return result(out, "number", err, "number", "title", "state", "html_url", "body")
	case "issue.comment":
		if p.Number <= 0 || p.Body == "" {
			return connectors.Result{}, fmt.Errorf("%w: issue.comment needs number and body", connectors.ErrPermanent)
		}
		err = a.do(ctx, http.MethodPost, rp+"/issues/"+strconv.Itoa(p.Number)+"/comments", "", map[string]any{"body": p.Body + "\n\n" + marker(operationID)}, &out)
		return result(out, "id", err, "id", "html_url")
	}
	return connectors.Result{}, fmt.Errorf("%w: unknown operation %q", connectors.ErrPermanent, op)
}

// FindByOperation looks for a pull request or comment carrying the marker.
func (a *Adapter) FindByOperation(ctx context.Context, op string, raw json.RawMessage, operationID string) (*connectors.Result, error) {
	var p params
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("%w: params: %v", connectors.ErrPermanent, err)
	}
	rp, err := repoPath(p.Repo)
	if err != nil {
		return nil, err
	}
	m := marker(operationID)
	switch op {
	case "pull_request.create":
		var list []map[string]any
		if err := a.do(ctx, http.MethodGet, rp+"/pulls?state=all&per_page=100", "", nil, &list); err != nil {
			return nil, err
		}
		for _, pr := range list {
			if b, _ := pr["body"].(string); strings.Contains(b, m) {
				r, err := result(pr, "number", nil, "number", "html_url", "state")
				return &r, err
			}
		}
	case "issue.comment":
		var list []map[string]any
		if err := a.do(ctx, http.MethodGet, rp+"/issues/"+strconv.Itoa(p.Number)+"/comments?per_page=100", "", nil, &list); err != nil {
			return nil, err
		}
		for _, c := range list {
			if b, _ := c["body"].(string); strings.Contains(b, m) {
				r, err := result(c, "id", nil, "id", "html_url")
				return &r, err
			}
		}
	}
	return nil, nil
}

func firstOr(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// result keeps the named fields of a GitHub object; receipt is its id field.
func result(obj map[string]any, receipt string, err error, keep ...string) (connectors.Result, error) {
	if err != nil {
		return connectors.Result{}, err
	}
	out := map[string]any{}
	for _, k := range keep {
		if v, ok := obj[k]; ok {
			if m, ok := v.(map[string]any); ok {
				v = m["ref"]
			}
			out[k] = v
		}
	}
	b, _ := json.Marshal(out)
	return connectors.Result{Receipt: fmt.Sprint(obj[receipt]), Data: b}, nil
}

func listResult(list []map[string]any, err error, keep ...string) (connectors.Result, error) {
	if err != nil {
		return connectors.Result{}, err
	}
	var items []json.RawMessage
	for _, o := range list {
		r, _ := result(o, "number", nil, keep...)
		items = append(items, r.Data)
	}
	b, _ := json.Marshal(map[string]any{"items": items})
	return connectors.Result{Receipt: strconv.Itoa(len(items)), Data: b}, nil
}
