// Package github is a fake of the GitHub REST API subset Steadmesh uses:
// personal access tokens, a GitHub App (RS256 JWTs, scoped installation
// tokens, revocation), repositories, pull requests and issue comments.
// Installation tokens are enforced: each works only for its repositories
// and permissions, until it expires or is revoked.
//
// Test endpoints:
//
//	GET  /_test/state      tokens, pull requests, comments and recorded requests
//	POST /_test/app        {"app_id","installation_id","public_key"} configures the App
//	POST /_test/tokens     {"valid":[...]} replaces the accepted PATs
//	POST /_test/reset      clears everything but the configuration
package github

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Token is an issued installation token.
type Token struct {
	Token       string            `json:"token"`
	Repos       []string          `json:"repos,omitempty"` // names; empty: every installation repository
	Permissions map[string]string `json:"permissions,omitempty"`
	ExpiresAt   time.Time         `json:"expires_at"`
	Revoked     bool              `json:"revoked"`
}

// Pull is a pull request.
type Pull struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	Head   string `json:"head"`
	Base   string `json:"base"`
	Body   string `json:"body"`
	State  string `json:"state"`
	Author string `json:"author"`
}

// Comment is an issue comment.
type Comment struct {
	ID     int    `json:"id"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Body   string `json:"body"`
}

// Request is a recorded API request.
type Request struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Auth   string `json:"auth"` // pat:<token>, app, inst:<token>, or none
	Status int    `json:"status"`
}

// State is the full fake state.
type State struct {
	Tokens   []Token   `json:"tokens"`
	Pulls    []Pull    `json:"pulls"`
	Comments []Comment `json:"comments"`
	Requests []Request `json:"requests"`
}

// Server is the fake.
type Server struct {
	mu       sync.Mutex
	owner    string
	repos    []string // owner/name
	pats     map[string]bool
	appID    string
	instID   string
	pub      *rsa.PublicKey
	tokens   []*Token
	pulls    []*Pull
	comments []*Comment
	reqs     []Request
	seq      int
	now      func() time.Time
}

// New returns a fake with the repositories owner/name and accepted PATs.
func New(repos []string, pats ...string) *Server {
	s := &Server{repos: repos, pats: map[string]bool{}, now: time.Now}
	for _, p := range pats {
		s.pats[p] = true
	}
	return s
}

// SetApp configures the GitHub App whose JWTs are accepted.
func (s *Server) SetApp(appID, installationID string, pub *rsa.PublicKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appID, s.instID, s.pub = appID, installationID, pub
}

// State returns a snapshot.
func (s *Server) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := State{Requests: append([]Request(nil), s.reqs...)}
	for _, t := range s.tokens {
		st.Tokens = append(st.Tokens, *t)
	}
	for _, p := range s.pulls {
		st.Pulls = append(st.Pulls, *p)
	}
	for _, c := range s.comments {
		st.Comments = append(st.Comments, *c)
	}
	return st
}

type caller struct {
	kind string // pat, app, inst
	tok  *Token
	pat  string
}

func (s *Server) json(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/_test/") {
		s.serveTest(w, r)
		return
	}
	rec := &statusRecorder{ResponseWriter: w, code: 200}
	c, authName := s.authenticate(r)
	s.serve(rec, r, c)
	s.mu.Lock()
	s.reqs = append(s.reqs, Request{Method: r.Method, Path: r.URL.Path, Auth: authName, Status: rec.code})
	s.mu.Unlock()
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) { r.code = code; r.ResponseWriter.WriteHeader(code) }

func (s *Server) authenticate(r *http.Request) (*caller, string) {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" {
		tok, _ = strings.CutPrefix(r.Header.Get("Authorization"), "token ")
	}
	if tok == "" {
		if _, pw, ok := r.BasicAuth(); ok {
			tok = pw
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case tok == "":
		return nil, "none"
	case s.pats[tok]:
		return &caller{kind: "pat", pat: tok}, "pat:" + tok
	case strings.Count(tok, ".") == 2:
		if s.verifyJWT(tok) {
			return &caller{kind: "app"}, "app"
		}
		return nil, "bad-jwt"
	}
	for _, t := range s.tokens {
		if t.Token == tok {
			if t.Revoked || s.now().After(t.ExpiresAt) {
				return nil, "inst-dead:" + tok
			}
			return &caller{kind: "inst", tok: t}, "inst:" + tok
		}
	}
	return nil, "unknown"
}

func (s *Server) verifyJWT(tok string) bool {
	if s.pub == nil {
		return false
	}
	parts := strings.Split(tok, ".")
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(s.pub, crypto.SHA256, sum[:], sig) != nil {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		Iss any   `json:"iss"`
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(b, &claims) != nil {
		return false
	}
	return fmt.Sprint(claims.Iss) == s.appID && s.now().Unix() < claims.Exp
}

// allowed reports whether c may access repo (owner/name) with permission perm at level.
func (s *Server) allowed(c *caller, repo, perm, level string) bool {
	if c == nil || c.kind == "app" {
		return false
	}
	if c.kind == "pat" {
		return true
	}
	_, name, _ := strings.Cut(repo, "/")
	if len(c.tok.Repos) > 0 && !slices.Contains(c.tok.Repos, name) {
		return false
	}
	if c.tok.Permissions == nil || perm == "" {
		return true
	}
	got := c.tok.Permissions[perm]
	return got == "write" || (got == "read" && level == "read")
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request, c *caller) {
	path := strings.TrimSuffix(r.URL.Path, "/")
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	notFound := func() { s.json(w, http.StatusNotFound, map[string]string{"message": "Not Found"}) }
	unauth := func() { s.json(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"}) }
	switch {
	case path == "/user" && r.Method == http.MethodGet:
		if c == nil || c.kind == "app" {
			unauth()
			return
		}
		s.json(w, 200, map[string]any{"login": "steadmesh-bot"})
	case path == "/app" && r.Method == http.MethodGet:
		if c == nil || c.kind != "app" {
			unauth()
			return
		}
		s.json(w, 200, map[string]any{"id": s.appID, "slug": "steadmesh-test"})
	case len(parts) == 4 && parts[0] == "app" && parts[1] == "installations" && parts[3] == "access_tokens" && r.Method == http.MethodPost:
		if c == nil || c.kind != "app" || parts[2] != s.instID {
			unauth()
			return
		}
		var body struct {
			Repositories []string          `json:"repositories"`
			Permissions  map[string]string `json:"permissions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.seq++
		t := &Token{Token: fmt.Sprintf("ghs_fake_%04d", s.seq), Repos: body.Repositories, ExpiresAt: s.now().Add(time.Hour).UTC().Truncate(time.Second)}
		if len(body.Permissions) > 0 {
			t.Permissions = body.Permissions
		}
		s.tokens = append(s.tokens, t)
		s.mu.Unlock()
		s.json(w, 201, map[string]any{"token": t.Token, "expires_at": t.ExpiresAt})
	case path == "/installation/token" && r.Method == http.MethodDelete:
		if c == nil || c.kind != "inst" {
			unauth()
			return
		}
		s.mu.Lock()
		c.tok.Revoked = true
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case len(parts) >= 3 && parts[0] == "repos":
		repo := parts[1] + "/" + parts[2]
		if !slices.Contains(s.repos, repo) {
			notFound()
			return
		}
		if c == nil {
			unauth()
			return
		}
		s.serveRepo(w, r, c, repo, parts[3:], notFound)
	default:
		notFound()
	}
}

func (s *Server) serveRepo(w http.ResponseWriter, r *http.Request, c *caller, repo string, rest []string, notFound func()) {
	forbid := func() {
		s.json(w, http.StatusForbidden, map[string]string{"message": "Resource not accessible by integration"})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		if !s.allowed(c, repo, "metadata", "read") && !s.allowed(c, repo, "contents", "read") {
			notFound()
			return
		}
		s.json(w, 200, map[string]any{"full_name": repo, "default_branch": "main", "private": true, "html_url": "https://github.test/" + repo})
	case len(rest) == 1 && rest[0] == "pulls" && r.Method == http.MethodGet:
		if !s.allowed(c, repo, "pull_requests", "read") {
			forbid()
			return
		}
		var out []map[string]any
		for _, p := range s.pulls {
			if p.Repo == repo {
				out = append(out, pullJSON(p))
			}
		}
		s.json(w, 200, out)
	case len(rest) == 1 && rest[0] == "pulls" && r.Method == http.MethodPost:
		if !s.allowed(c, repo, "pull_requests", "write") {
			forbid()
			return
		}
		var in Pull
		_ = json.NewDecoder(r.Body).Decode(&in)
		s.seq++
		p := &Pull{Repo: repo, Number: s.seq, Title: in.Title, Head: in.Head, Base: in.Base, Body: in.Body, State: "open", Author: c.kind}
		s.pulls = append(s.pulls, p)
		s.json(w, 201, pullJSON(p))
	case len(rest) == 2 && rest[0] == "pulls" && r.Method == http.MethodGet:
		n, _ := strconv.Atoi(rest[1])
		for _, p := range s.pulls {
			if p.Repo == repo && p.Number == n && s.allowed(c, repo, "pull_requests", "read") {
				s.json(w, 200, pullJSON(p))
				return
			}
		}
		notFound()
	case len(rest) == 3 && rest[0] == "issues" && rest[2] == "comments":
		n, _ := strconv.Atoi(rest[1])
		if r.Method == http.MethodPost {
			if !s.allowed(c, repo, "issues", "write") && !s.allowed(c, repo, "pull_requests", "write") {
				forbid()
				return
			}
			var in Comment
			_ = json.NewDecoder(r.Body).Decode(&in)
			s.seq++
			cm := &Comment{ID: s.seq, Repo: repo, Number: n, Body: in.Body}
			s.comments = append(s.comments, cm)
			s.json(w, 201, map[string]any{"id": cm.ID, "body": cm.Body, "html_url": fmt.Sprintf("https://github.test/%s/issues/%d#c%d", repo, n, cm.ID)})
			return
		}
		var out []map[string]any
		for _, cm := range s.comments {
			if cm.Repo == repo && cm.Number == n {
				out = append(out, map[string]any{"id": cm.ID, "body": cm.Body})
			}
		}
		s.json(w, 200, out)
	default:
		notFound()
	}
}

func pullJSON(p *Pull) map[string]any {
	return map[string]any{"number": p.Number, "title": p.Title, "state": p.State, "body": p.Body,
		"head": map[string]any{"ref": p.Head}, "base": map[string]any{"ref": p.Base},
		"html_url": fmt.Sprintf("https://github.test/%s/pull/%d", p.Repo, p.Number)}
}

func (s *Server) serveTest(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/_test/state":
		s.json(w, 200, s.State())
	case "/_test/tokens":
		var body struct {
			Valid []string `json:"valid"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		s.mu.Lock()
		s.pats = map[string]bool{}
		for _, t := range body.Valid {
			s.pats[t] = true
		}
		s.mu.Unlock()
		s.json(w, 200, map[string]bool{"ok": true})
	case "/_test/app":
		var body struct {
			AppID          string `json:"app_id"`
			InstallationID string `json:"installation_id"`
			PublicKey      string `json:"public_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		block, _ := pem.Decode([]byte(body.PublicKey))
		if block == nil {
			http.Error(w, "public_key is not PEM", 400)
			return
		}
		k, err := x509.ParsePKIXPublicKey(block.Bytes)
		pub, ok := k.(*rsa.PublicKey)
		if err != nil || !ok {
			http.Error(w, "public_key is not an RSA PKIX key", 400)
			return
		}
		s.SetApp(body.AppID, body.InstallationID, pub)
		s.json(w, 200, map[string]bool{"ok": true})
	case "/_test/reset":
		s.mu.Lock()
		s.tokens, s.pulls, s.comments, s.reqs = nil, nil, nil, nil
		s.mu.Unlock()
		s.json(w, 200, map[string]bool{"ok": true})
	default:
		http.NotFound(w, r)
	}
}
