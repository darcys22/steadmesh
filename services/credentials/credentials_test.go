package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/pkg/access"
)

type issuer struct {
	revocable bool
	n         int
	revoked   []string
}

func (i *issuer) Verify(context.Context) error { return nil }
func (i *issuer) Invoke(context.Context, string, json.RawMessage, string) (connectors.Result, error) {
	return connectors.Result{}, nil
}
func (i *issuer) FindByOperation(context.Context, string, json.RawMessage, string) (*connectors.Result, error) {
	return nil, nil
}
func (i *issuer) ReadOnly(string) bool { return true }
func (i *issuer) IssueCredential(_ context.Context, s connectors.CredentialScope) (connectors.Credential, error) {
	i.n++
	c := connectors.Credential{Username: "x-access-token", Token: fmt.Sprintf("tok-%d-%v", i.n, s.Repos), Revocable: i.revocable}
	if i.revocable {
		c.ExpiresAt = time.Now().Add(time.Hour)
	}
	return c, nil
}
func (i *issuer) RevokeCredential(_ context.Context, tok string) error {
	i.revoked = append(i.revoked, tok)
	return nil
}

type conns map[string]connectors.Tracker

func (c conns) Tracker(_, key string) (connectors.Tracker, error) {
	if t, ok := c[key]; ok {
		return t, nil
	}
	return nil, errors.New("no such connection")
}

func grant(delivery string) *access.SeatAccess {
	return &access.SeatAccess{GitHub: []access.GitHubGrant{{Connection: "github", Repos: []string{"acme/a"}, Permissions: map[string]string{"contents": "write"}, Delivery: delivery}}}
}

func TestIssueOnlyWhenGranted(t *testing.T) {
	gh := &issuer{revocable: true}
	r := &Registry{Connections: conns{"github": gh}}
	ctx := context.Background()
	c, err := r.Issue(ctx, "org", "engineer", grant(access.DeliverySandbox), "github")
	if err != nil || c.Token != "tok-1-[acme/a]" || !c.Revocable {
		t.Fatalf("issue %+v %v", c, err)
	}
	for name, a := range map[string]*access.SeatAccess{"platform delivery": grant(access.DeliveryPlatform), "no access": nil} {
		if _, err := r.Issue(ctx, "org", "engineer", a, "github"); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := r.Issue(ctx, "org", "engineer", grant(access.DeliverySandbox), "other"); !errors.Is(err, ErrForbidden) {
		t.Errorf("other connection: %v", err)
	}
	if r.Outstanding("org", "engineer") != 1 {
		t.Fatal("revocable credential not remembered")
	}
}

func TestReconcileRevokesLostGrants(t *testing.T) {
	gh := &issuer{revocable: true}
	r := &Registry{Connections: conns{"github": gh}}
	ctx := context.Background()
	for _, seat := range []string{"engineer", "reviewer"} {
		if _, err := r.Issue(ctx, "org", seat, grant(access.DeliverySandbox), "github"); err != nil {
			t.Fatal(err)
		}
	}
	// The reviewer keeps its grant; the engineer loses it; another org is untouched.
	out := r.Reconcile(ctx, "org", map[string]*access.SeatAccess{"reviewer": grant(access.DeliverySandbox), "engineer": nil})
	if len(out) != 1 || out[0].Seat != "engineer" || len(gh.revoked) != 1 || gh.revoked[0] != "tok-1-[acme/a]" {
		t.Fatalf("revoked %+v %v", out, gh.revoked)
	}
	if r.Outstanding("org", "engineer") != 0 || r.Outstanding("org", "reviewer") != 1 {
		t.Fatal("registry state after reconcile")
	}
	// A retired seat (missing from the manifest) loses its credentials.
	r.Reconcile(ctx, "org", map[string]*access.SeatAccess{})
	if len(gh.revoked) != 2 {
		t.Fatalf("revoked %v", gh.revoked)
	}
}

func TestUnrevocableCredentialsAreNotTracked(t *testing.T) {
	pat := &issuer{}
	r := &Registry{Connections: conns{"github": pat}}
	if _, err := r.Issue(context.Background(), "org", "engineer", grant(access.DeliverySandbox), "github"); err != nil {
		t.Fatal(err)
	}
	if r.Outstanding("org", "engineer") != 0 || len(r.Reconcile(context.Background(), "org", nil)) != 0 {
		t.Fatal("a PAT was tracked for revocation")
	}
}
