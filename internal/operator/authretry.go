/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"context"
	"fmt"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// retryUnauthorizedClient wraps a tenant client so a rejected credential costs
// one retry instead of a whole reconcile.
//
// A TokenRequest token is bound to the ServiceAccount's UID; tokenCache is
// keyed by its name. A ServiceAccount deleted and recreated under the same name
// — what tearing a namespace down and rebuilding it does to every SA in it —
// therefore leaves a cached token authenticating as an object the apiserver no
// longer knows, and every call through it answers 401 until the entry reaches
// its TTL an hour later. tenantClientCache invalidates on the token *string*
// changing, so it keeps handing back the client built around the dead one. A
// revoked token and a rotated signing key have the identical shape.
//
// 401 is the one error that means the cached credential is worthless, so it is
// the one error worth treating as an eviction signal: refresh drops the cached
// token (and the client built around it), mints a fresh one, and the operation
// runs exactly once more. Authentication precedes admission, so a 401 is proof
// the request had no effect — replaying a mutation after one cannot duplicate
// it.
//
// The retry is bounded to a single attempt: a tenant that genuinely cannot
// authenticate must surface the failure rather than spin minting tokens.
type retryUnauthorizedClient struct {
	// refresh evicts the cached credential for the tenant and returns a client
	// built around a freshly minted one.
	refresh func(ctx context.Context) (client.Client, error)

	namespace      string
	serviceAccount string

	mu      sync.RWMutex
	current client.Client
}

// newRetryUnauthorizedClient wraps inner, which must be a client built from a
// token minted for namespace/serviceAccount.
func newRetryUnauthorizedClient(inner client.Client, namespace, serviceAccount string, refresh func(context.Context) (client.Client, error)) *retryUnauthorizedClient {
	return &retryUnauthorizedClient{
		refresh:        refresh,
		namespace:      namespace,
		serviceAccount: serviceAccount,
		current:        inner,
	}
}

// inner returns the client calls currently run through — the one built at
// construction, or its replacement once a 401 forced a re-mint.
func (c *retryUnauthorizedClient) inner() client.Client {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.current
}

// do runs op against the current tenant client. On 401 it refreshes the
// credential and runs op once more; every other outcome is returned unchanged.
func (c *retryUnauthorizedClient) do(ctx context.Context, op func(client.Client) error) error {
	err := op(c.inner())
	if !apierrors.IsUnauthorized(err) {
		return err
	}
	fresh, refreshErr := c.refresh(ctx)
	if refreshErr != nil {
		// The 401 stays at the head of the chain so callers still classify this
		// as an authentication failure rather than as a minting failure.
		return fmt.Errorf("%w (re-minting a token for ServiceAccount %s/%s failed: %v)",
			err, c.namespace, c.serviceAccount, refreshErr)
	}
	c.mu.Lock()
	c.current = fresh
	c.mu.Unlock()
	return c.explain(op(fresh))
}

// explain decorates a 401 that survived a re-mint. On its own "Unauthorized"
// reads as a broken RoleBinding and sends operators to fix RBAC that is already
// correct; naming the credential as the suspect points at the actual cause.
func (c *retryUnauthorizedClient) explain(err error) error {
	if !apierrors.IsUnauthorized(err) {
		return err
	}
	return fmt.Errorf("%w (a freshly minted token for ServiceAccount %s/%s was refused too — the ServiceAccount may no longer exist, or its namespace is terminating)",
		err, c.namespace, c.serviceAccount)
}

func (c *retryUnauthorizedClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return c.do(ctx, func(cl client.Client) error { return cl.Get(ctx, key, obj, opts...) })
}

func (c *retryUnauthorizedClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return c.do(ctx, func(cl client.Client) error { return cl.List(ctx, list, opts...) })
}

func (c *retryUnauthorizedClient) Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
	return c.do(ctx, func(cl client.Client) error { return cl.Apply(ctx, obj, opts...) })
}

func (c *retryUnauthorizedClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	return c.do(ctx, func(cl client.Client) error { return cl.Create(ctx, obj, opts...) })
}

func (c *retryUnauthorizedClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	return c.do(ctx, func(cl client.Client) error { return cl.Delete(ctx, obj, opts...) })
}

func (c *retryUnauthorizedClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	return c.do(ctx, func(cl client.Client) error { return cl.Update(ctx, obj, opts...) })
}

func (c *retryUnauthorizedClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	return c.do(ctx, func(cl client.Client) error { return cl.Patch(ctx, obj, patch, opts...) })
}

func (c *retryUnauthorizedClient) DeleteAllOf(ctx context.Context, obj client.Object, opts ...client.DeleteAllOfOption) error {
	return c.do(ctx, func(cl client.Client) error { return cl.DeleteAllOf(ctx, obj, opts...) })
}

// Status mirrors controller-runtime's own client, whose Status() is the
// "status" subresource client.
func (c *retryUnauthorizedClient) Status() client.SubResourceWriter {
	return c.SubResource("status")
}

func (c *retryUnauthorizedClient) SubResource(subResource string) client.SubResourceClient {
	return &retryUnauthorizedSubResourceClient{parent: c, subResource: subResource}
}

// Scheme, RESTMapper, GroupVersionKindFor and IsObjectNamespaced are local
// lookups that never reach the apiserver, so they delegate without the retry.

func (c *retryUnauthorizedClient) Scheme() *runtime.Scheme { return c.inner().Scheme() }

func (c *retryUnauthorizedClient) RESTMapper() apimeta.RESTMapper { return c.inner().RESTMapper() }

func (c *retryUnauthorizedClient) GroupVersionKindFor(obj runtime.Object) (schema.GroupVersionKind, error) {
	return c.inner().GroupVersionKindFor(obj)
}

func (c *retryUnauthorizedClient) IsObjectNamespaced(obj runtime.Object) (bool, error) {
	return c.inner().IsObjectNamespaced(obj)
}

// retryUnauthorizedSubResourceClient routes subresource calls through the
// parent's do, so a stale credential is refreshed for them on the same terms as
// a top-level call.
type retryUnauthorizedSubResourceClient struct {
	parent      *retryUnauthorizedClient
	subResource string
}

func (s *retryUnauthorizedSubResourceClient) Get(ctx context.Context, obj, subResource client.Object, opts ...client.SubResourceGetOption) error {
	return s.parent.do(ctx, func(cl client.Client) error {
		return cl.SubResource(s.subResource).Get(ctx, obj, subResource, opts...)
	})
}

func (s *retryUnauthorizedSubResourceClient) Create(ctx context.Context, obj, subResource client.Object, opts ...client.SubResourceCreateOption) error {
	return s.parent.do(ctx, func(cl client.Client) error {
		return cl.SubResource(s.subResource).Create(ctx, obj, subResource, opts...)
	})
}

func (s *retryUnauthorizedSubResourceClient) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	return s.parent.do(ctx, func(cl client.Client) error {
		return cl.SubResource(s.subResource).Update(ctx, obj, opts...)
	})
}

func (s *retryUnauthorizedSubResourceClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	return s.parent.do(ctx, func(cl client.Client) error {
		return cl.SubResource(s.subResource).Patch(ctx, obj, patch, opts...)
	})
}

func (s *retryUnauthorizedSubResourceClient) Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.SubResourceApplyOption) error {
	return s.parent.do(ctx, func(cl client.Client) error {
		return cl.SubResource(s.subResource).Apply(ctx, obj, opts...)
	})
}

// Compile-time proof the wrappers satisfy the interfaces they stand in for.
var (
	_ client.Client            = (*retryUnauthorizedClient)(nil)
	_ client.SubResourceClient = (*retryUnauthorizedSubResourceClient)(nil)
)
