package auth_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	authnv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/darcys22/steadmesh/services/auth"
)

const controllerUser = "system:serviceaccount:steadmesh-system:steadmesh-controller"

type seats map[string]string

func (s seats) SeatByServiceAccount(_ context.Context, ns, sa string) (string, error) {
	id, ok := s[ns+"/"+sa]
	if !ok {
		return "", errors.New("not found")
	}
	return id, nil
}

// tokens maps a token to the username it authenticates as, per audience.
type token struct {
	user      string
	audiences []string
	pod       string
}

func newAuth(t *testing.T, tokens map[string]token, reviews *int) *auth.TokenReview {
	t.Helper()
	cs := fake.NewClientset()
	cs.PrependReactor("create", "tokenreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		*reviews++
		tr := a.(k8stesting.CreateAction).GetObject().(*authnv1.TokenReview)
		tok, ok := tokens[tr.Spec.Token]
		want := tr.Spec.Audiences
		if len(want) == 0 {
			want = []string{"https://kubernetes.default.svc"}
		}
		if !ok || !slices.Equal(tok.audiences, want) {
			tr.Status = authnv1.TokenReviewStatus{Authenticated: false}
			return true, tr, nil
		}
		tr.Status = authnv1.TokenReviewStatus{Authenticated: true, Audiences: tok.audiences,
			User: authnv1.UserInfo{Username: tok.user, Extra: map[string]authnv1.ExtraValue{auth.PodUIDExtra: {tok.pod}}}}
		return true, tr, nil
	})
	return auth.NewTokenReview(cs, seats{"acme/seat-lead-1234": "seat-id-lead"}, controllerUser)
}

func TestSeatAuthentication(t *testing.T) {
	ctx := context.Background()
	var reviews int
	a := newAuth(t, map[string]token{
		"seat":       {user: "system:serviceaccount:acme:seat-lead-1234", audiences: []string{"steadmesh-gateway"}, pod: "pod-1"},
		"unknown":    {user: "system:serviceaccount:acme:other", audiences: []string{"steadmesh-gateway"}},
		"controller": {user: controllerUser, audiences: []string{"https://kubernetes.default.svc"}},
	}, &reviews)

	s, err := a.Seat(ctx, "seat")
	if err != nil || s.SeatID != "seat-id-lead" || s.PodUID != "pod-1" {
		t.Fatalf("seat = %+v, %v", s, err)
	}
	if _, err := a.Seat(ctx, "seat"); err != nil || reviews != 1 {
		t.Fatalf("expected cached review, reviews=%d err=%v", reviews, err)
	}
	if _, err := a.Seat(ctx, "unknown"); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("unmapped service account: %v", err)
	}
	if _, err := a.Seat(ctx, "controller"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("controller token accepted as seat: %v", err)
	}
	if _, err := a.Seat(ctx, ""); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("empty token: %v", err)
	}
}

func TestControllerAuthentication(t *testing.T) {
	ctx := context.Background()
	var reviews int
	a := newAuth(t, map[string]token{
		"seat":       {user: "system:serviceaccount:acme:seat-lead-1234", audiences: []string{"steadmesh-gateway"}},
		"controller": {user: controllerUser, audiences: []string{"https://kubernetes.default.svc"}},
		"other":      {user: "system:serviceaccount:default:builder", audiences: []string{"https://kubernetes.default.svc"}},
	}, &reviews)
	if err := a.Controller(ctx, "controller"); err != nil {
		t.Fatal(err)
	}
	if err := a.Controller(ctx, "seat"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("seat token accepted on internal API: %v", err)
	}
	if err := a.Controller(ctx, "other"); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("non-allow-listed identity: %v", err)
	}
}
