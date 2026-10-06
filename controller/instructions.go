package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/darcys22/steadmesh/pkg/compile"
)

// MaxInstructionBytes bounds a single referenced instruction document.
const MaxInstructionBytes = 1 << 20

// Instruction error reasons (Configured=False reasons).
const (
	ReasonInstructionDigestMismatch = "InstructionDigestMismatch"
	ReasonInstructionUnavailable    = "InstructionUnavailable"
	ReasonInstructionInvalid        = "InstructionRefInvalid"
)

// InstructionError is a failure to resolve or verify an instruction reference.
type InstructionError struct {
	Ref    string
	Reason string
	Detail string
}

func (e *InstructionError) Error() string { return fmt.Sprintf("%s: %s", e.Ref, e.Detail) }

// InstructionResolver fetches instruction bundles and verifies their digests.
// configmap:<name>/<key> refs are read live from the organisation namespace;
// https refs are fetched once and cached by ref (the digest makes them immutable).
type InstructionResolver struct {
	Reader client.Reader
	HTTP   *http.Client

	mu    sync.Mutex
	https map[string]string
}

func NewInstructionResolver(r client.Reader, hc *http.Client) *InstructionResolver {
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &InstructionResolver{Reader: r, HTTP: hc, https: map[string]string{}}
}

func splitRef(ref string) (loc, digest string, err error) {
	loc, digest, ok := strings.Cut(ref, "#sha256:")
	if !ok || len(digest) != 64 {
		return "", "", &InstructionError{Ref: ref, Reason: ReasonInstructionInvalid, Detail: "missing #sha256:<digest>"}
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return "", "", &InstructionError{Ref: ref, Reason: ReasonInstructionInvalid, Detail: "digest is not hex"}
	}
	return loc, strings.ToLower(digest), nil
}

func verifyDigest(ref string, content []byte, want string) error {
	sum := sha256.Sum256(content)
	if got := hex.EncodeToString(sum[:]); got != want {
		return &InstructionError{Ref: ref, Reason: ReasonInstructionDigestMismatch, Detail: fmt.Sprintf("content digest sha256:%s does not match the declared sha256:%s", got, want)}
	}
	return nil
}

// Resolve returns the verified text of ref.
func (r *InstructionResolver) Resolve(ctx context.Context, namespace, ref string) (string, error) {
	loc, digest, err := splitRef(ref)
	if err != nil {
		return "", err
	}
	switch {
	case strings.HasPrefix(loc, "configmap:"):
		name, key, ok := strings.Cut(strings.TrimPrefix(loc, "configmap:"), "/")
		if !ok {
			return "", &InstructionError{Ref: ref, Reason: ReasonInstructionInvalid, Detail: "must be configmap:<name>/<key>"}
		}
		var cm corev1.ConfigMap
		if err := r.Reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &cm); err != nil {
			if apierrors.IsNotFound(err) {
				return "", &InstructionError{Ref: ref, Reason: ReasonInstructionUnavailable, Detail: fmt.Sprintf("ConfigMap %s/%s not found", namespace, name)}
			}
			return "", &InstructionError{Ref: ref, Reason: ReasonInstructionUnavailable, Detail: err.Error()}
		}
		var content []byte
		if v, ok := cm.Data[key]; ok {
			content = []byte(v)
		} else if v, ok := cm.BinaryData[key]; ok {
			content = v
		} else {
			return "", &InstructionError{Ref: ref, Reason: ReasonInstructionUnavailable, Detail: fmt.Sprintf("ConfigMap %s/%s has no key %q", namespace, name, key)}
		}
		if err := verifyDigest(ref, content, digest); err != nil {
			return "", err
		}
		return string(content), nil
	case strings.HasPrefix(loc, "https://"):
		r.mu.Lock()
		if v, ok := r.https[ref]; ok {
			r.mu.Unlock()
			return v, nil
		}
		r.mu.Unlock()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, loc, nil)
		if err != nil {
			return "", &InstructionError{Ref: ref, Reason: ReasonInstructionInvalid, Detail: err.Error()}
		}
		resp, err := r.HTTP.Do(req)
		if err != nil {
			return "", &InstructionError{Ref: ref, Reason: ReasonInstructionUnavailable, Detail: err.Error()}
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", &InstructionError{Ref: ref, Reason: ReasonInstructionUnavailable, Detail: "HTTP " + resp.Status}
		}
		content, err := io.ReadAll(io.LimitReader(resp.Body, MaxInstructionBytes+1))
		if err != nil {
			return "", &InstructionError{Ref: ref, Reason: ReasonInstructionUnavailable, Detail: err.Error()}
		}
		if len(content) > MaxInstructionBytes {
			return "", &InstructionError{Ref: ref, Reason: ReasonInstructionInvalid, Detail: "document exceeds 1 MiB"}
		}
		if err := verifyDigest(ref, content, digest); err != nil {
			return "", err
		}
		r.mu.Lock()
		r.https[ref] = string(content)
		r.mu.Unlock()
		return string(content), nil
	default:
		return "", &InstructionError{Ref: ref, Reason: ReasonInstructionInvalid, Detail: "unsupported reference scheme"}
	}
}

// ResolveAll resolves every instruction of the manifest's seats. It returns
// all failures; the first error's reason is the Configured reason.
func (r *InstructionResolver) ResolveAll(ctx context.Context, namespace string, m *compile.Manifest) (map[string]string, []error) {
	texts := map[string]string{}
	var errs []error
	seen := map[string]bool{}
	for _, k := range sortedKeys(m.Seats) {
		for _, src := range m.Seats[k].Instructions {
			if seen[src.Ref] {
				continue
			}
			seen[src.Ref] = true
			t, err := r.Resolve(ctx, namespace, src.Ref)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			texts[src.Ref] = t
		}
	}
	return texts, errs
}

// instructionReason returns the condition reason for resolution errors.
func instructionReason(errs []error) string {
	for _, err := range errs {
		var ie *InstructionError
		if errors.As(err, &ie) && ie.Reason == ReasonInstructionDigestMismatch {
			return ie.Reason
		}
	}
	var ie *InstructionError
	if len(errs) > 0 && errors.As(errs[0], &ie) {
		return ie.Reason
	}
	return ReasonInstructionUnavailable
}

func scopeTitle(scope string) string {
	switch {
	case scope == "organisation":
		return "Organisation"
	case strings.HasPrefix(scope, "team:"):
		return "Team " + strings.TrimPrefix(scope, "team:")
	case scope == "role":
		return "Role"
	case scope == "seat":
		return "Seat"
	}
	return scope
}

// RenderInstructions concatenates instruction texts in manifest order, each
// under a header naming its scope and source (§4.3).
func RenderInstructions(sources []compile.InstructionSource, texts map[string]string) string {
	var b strings.Builder
	for i, s := range sources {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "<!-- steadmesh: order=%d scope=%s ref=%s -->\n", s.Order, s.Scope, s.Ref)
		fmt.Fprintf(&b, "# %s\n\n", scopeTitle(s.Scope))
		b.WriteString(strings.TrimRight(texts[s.Ref], "\n"))
		b.WriteString("\n")
	}
	return b.String()
}
