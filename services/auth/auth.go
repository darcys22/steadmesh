// Package auth authenticates the platform's callers (§13.2). Seats present a
// projected ServiceAccount token whose only audience is steadmesh-gateway; the
// controller presents its own ServiceAccount token with the default audience.
// Both are verified with a Kubernetes TokenReview, and the principal is
// derived from the verified username, never from request content.
package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/darcys22/steadmesh/api/v1alpha1"
)

var (
	ErrUnauthenticated = errors.New("unauthenticated")
	ErrForbidden       = errors.New("forbidden")
)

// PodUIDExtra is the TokenReview extra field carrying the bound Pod UID.
const PodUIDExtra = "authentication.kubernetes.io/pod-uid"

// Seat is an authenticated seat principal.
type Seat struct {
	SeatID string
	PodUID string
}

// Authenticator verifies bearer tokens.
type Authenticator interface {
	// Seat resolves a gateway-audience token to an active seat.
	Seat(ctx context.Context, token string) (Seat, error)
	// Controller verifies the controller's token.
	Controller(ctx context.Context, token string) error
}

// SeatResolver maps a verified ServiceAccount to its active seat id.
type SeatResolver interface {
	SeatByServiceAccount(ctx context.Context, namespace, serviceAccount string) (string, error)
}

// TokenReview is the production Authenticator.
type TokenReview struct {
	client     kubernetes.Interface
	seats      SeatResolver
	controller string
	cache      reviewCache
}

// NewTokenReview returns an Authenticator using client for TokenReviews.
// controllerUsername is the only identity accepted on the internal API, e.g.
// system:serviceaccount:steadmesh-system:steadmesh-controller.
func NewTokenReview(client kubernetes.Interface, seats SeatResolver, controllerUsername string) *TokenReview {
	return &TokenReview{client: client, seats: seats, controller: controllerUsername, cache: reviewCache{ttl: 30 * time.Second}}
}

type reviewed struct {
	username string
	podUID   string
}

func (a *TokenReview) review(ctx context.Context, token string, audiences []string) (reviewed, error) {
	if token == "" {
		return reviewed{}, ErrUnauthenticated
	}
	key := sha256.Sum256([]byte(strings.Join(audiences, ",") + "\x00" + token))
	if r, ok := a.cache.get(key); ok {
		return r, nil
	}
	res, err := a.client.AuthenticationV1().TokenReviews().Create(ctx, &authnv1.TokenReview{
		Spec: authnv1.TokenReviewSpec{Token: token, Audiences: audiences},
	}, metav1.CreateOptions{})
	if err != nil {
		return reviewed{}, fmt.Errorf("token review: %w", err)
	}
	st := res.Status
	if !st.Authenticated {
		return reviewed{}, ErrUnauthenticated
	}
	for _, want := range audiences {
		found := false
		for _, got := range st.Audiences {
			found = found || got == want
		}
		if !found {
			return reviewed{}, ErrUnauthenticated
		}
	}
	r := reviewed{username: st.User.Username}
	if pod := st.User.Extra[PodUIDExtra]; len(pod) > 0 {
		r.podUID = pod[0]
	}
	a.cache.put(key, r)
	return r, nil
}

// Seat implements Authenticator. The seat mapping is resolved on every call so
// a retired seat loses access immediately; only the TokenReview is cached.
func (a *TokenReview) Seat(ctx context.Context, token string) (Seat, error) {
	r, err := a.review(ctx, token, []string{v1alpha1.GatewayAudience})
	if err != nil {
		return Seat{}, err
	}
	ns, sa, ok := serviceAccount(r.username)
	if !ok {
		return Seat{}, ErrForbidden
	}
	id, err := a.seats.SeatByServiceAccount(ctx, ns, sa)
	if err != nil {
		return Seat{}, fmt.Errorf("%w: %s is not an active seat", ErrForbidden, r.username)
	}
	return Seat{SeatID: id, PodUID: r.podUID}, nil
}

// Controller implements Authenticator.
func (a *TokenReview) Controller(ctx context.Context, token string) error {
	r, err := a.review(ctx, token, nil)
	if err != nil {
		return err
	}
	if r.username != a.controller {
		return ErrForbidden
	}
	return nil
}

func serviceAccount(username string) (namespace, name string, ok bool) {
	rest, ok := strings.CutPrefix(username, "system:serviceaccount:")
	if !ok {
		return "", "", false
	}
	namespace, name, ok = strings.Cut(rest, ":")
	return namespace, name, ok && namespace != "" && name != ""
}

// reviewCache remembers successful reviews briefly to avoid a TokenReview per
// request. Tokens are keyed by hash and never stored.
type reviewCache struct {
	ttl time.Duration
	mu  sync.Mutex
	m   map[[32]byte]cacheEntry
}

type cacheEntry struct {
	r       reviewed
	expires time.Time
}

const maxCacheEntries = 4096

func (c *reviewCache) get(k [32]byte) (reviewed, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok || time.Now().After(e.expires) {
		return reviewed{}, false
	}
	return e.r, true
}

func (c *reviewCache) put(k [32]byte, r reviewed) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.m == nil {
		c.m = map[[32]byte]cacheEntry{}
	}
	if len(c.m) >= maxCacheEntries {
		for key, e := range c.m {
			if now.After(e.expires) || len(c.m) >= maxCacheEntries {
				delete(c.m, key)
			}
		}
	}
	c.m[k] = cacheEntry{r: r, expires: now.Add(c.ttl)}
}
