/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package selfsigned

import (
	"bytes"
	"context"
	"fmt"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
)

// VWCClient is the subset of the admissionregistration client surface
// the patcher needs. Production wraps clientset.AdmissionregistrationV1()
// .ValidatingWebhookConfigurations(); tests substitute a fake.
//
// The write is an Update (not a Patch) so it can carry the object's
// resourceVersion and get optimistic-concurrency semantics from the
// apiserver — see UpdateVWCCABundle.
type VWCClient interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*admissionv1.ValidatingWebhookConfiguration, error)
	Update(ctx context.Context, vwc *admissionv1.ValidatingWebhookConfiguration, opts metav1.UpdateOptions) (*admissionv1.ValidatingWebhookConfiguration, error)
}

// UpdateVWCCABundle reads the named VWC, applies mutate to its current
// caBundle (the first webhook entry's — every entry is kept in sync),
// stamps the result onto every webhook entry, and writes it back under
// optimistic concurrency.
//
// On a Conflict — a concurrent writer changed the object between our Get
// and Update — it re-reads and re-applies mutate. This is the load-bearing
// property for multi-replica self-signed installs: during a rolling update
// several pods bootstrap their CAs near-simultaneously, and a blind
// last-write-wins patch would drop a peer's just-added CA, breaking that
// replica's admission until its own next rotation. Because mutate is a pure
// set operation (mergeCABundle: add my CA, drop my old CA, prune expired)
// it is idempotent and commutative, so re-applying it against the winner's
// updated object CONVERGES — every replica's CA survives.
//
// Transient apiserver errors (5xx, throttled, ServerTimeout) are retried
// too so a brief hiccup during operator restart doesn't fail the bootstrap.
// A mutate that produces no change short-circuits without issuing an Update.
func UpdateVWCCABundle(ctx context.Context, c VWCClient, name string, mutate func(current []byte) []byte) error {
	if name == "" {
		return fmt.Errorf("selfsigned: UpdateVWCCABundle name required")
	}
	return retry.OnError(retry.DefaultBackoff, isRetriableVWCError, func() error {
		vwc, err := c.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("selfsigned: VWC %q not found — chart deployed?", name)
			}
			return err
		}
		if len(vwc.Webhooks) == 0 {
			return fmt.Errorf("selfsigned: VWC %q has no webhook entries", name)
		}
		next := mutate(vwc.Webhooks[0].ClientConfig.CABundle)
		if len(next) == 0 {
			return fmt.Errorf("selfsigned: refusing to write empty caBundle to VWC %q", name)
		}
		changed := false
		for i := range vwc.Webhooks {
			if !bytes.Equal(vwc.Webhooks[i].ClientConfig.CABundle, next) {
				vwc.Webhooks[i].ClientConfig.CABundle = next
				changed = true
			}
		}
		if !changed {
			return nil
		}
		_, err = c.Update(ctx, vwc, metav1.UpdateOptions{})
		return err
	})
}

// isRetriableVWCError reports whether an Update error is worth retrying:
// a Conflict (optimistic-concurrency loss to a concurrent writer) or a
// transient apiserver condition. Permanent errors (Forbidden, NotFound,
// the no-webhooks/empty-bundle guards above) fall through and surface.
func isRetriableVWCError(err error) bool {
	return apierrors.IsConflict(err) ||
		apierrors.IsServerTimeout(err) ||
		apierrors.IsServiceUnavailable(err) ||
		apierrors.IsTooManyRequests(err) ||
		apierrors.IsInternalError(err)
}
