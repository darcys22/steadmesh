package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/darcys22/steadmesh/api/v1alpha1"
)

// fakeClient is an in-memory orgClient for unit tests.
type fakeClient struct {
	mu       sync.Mutex
	objs     map[string]*v1alpha1.AgentOrganization
	getErr   error
	applyErr error
	applies  int
	// onApply may set status on the stored object.
	onApply func(*v1alpha1.AgentOrganization)
}

var gr = schema.GroupResource{Group: "steadmesh.io", Resource: "agentorganizations"}

func newFakeClient() *fakeClient {
	return &fakeClient{objs: map[string]*v1alpha1.AgentOrganization{}}
}

func (f *fakeClient) Get(_ context.Context, ns, name string) (*v1alpha1.AgentOrganization, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	o, ok := f.objs[ns+"/"+name]
	if !ok {
		return nil, apierrors.NewNotFound(gr, name)
	}
	return o.DeepCopy(), nil
}

func (f *fakeClient) Apply(_ context.Context, obj map[string]any) (*v1alpha1.AgentOrganization, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	f.applies++
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	var in v1alpha1.AgentOrganization
	if err := json.Unmarshal(b, &in); err != nil {
		return nil, err
	}
	k := in.Namespace + "/" + in.Name
	if cur, ok := f.objs[k]; ok {
		in.UID, in.Generation, in.Status = cur.UID, cur.Generation, cur.Status
		if string(canonicalSpecJSON(cur.Spec.OrganizationSpec)) != string(canonicalSpecJSON(in.Spec.OrganizationSpec)) {
			in.Generation++
		}
	} else {
		in.UID = types.UID(fmt.Sprintf("uid-%d", f.applies))
		in.Generation = 1
	}
	if f.onApply != nil {
		f.onApply(&in)
	}
	f.objs[k] = &in
	return in.DeepCopy(), nil
}

func (f *fakeClient) Delete(_ context.Context, ns, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := ns + "/" + name
	if _, ok := f.objs[k]; !ok {
		return apierrors.NewNotFound(gr, name)
	}
	delete(f.objs, k)
	return nil
}
