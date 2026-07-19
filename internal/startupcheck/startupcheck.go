/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

// Package startupcheck reports, at boot, which of the operator's watched
// resources are not yet reconcilable — a CRD that is not installed, or one the
// operator ServiceAccount may not list/watch. It only logs: the manager's large
// cache-sync timeout is what keeps the process alive and retrying, so a missing
// CRD or an incomplete ClusterRole degrades to "not ready, waiting" instead of a
// crash-loop, and self-heals the moment the prerequisite appears. This turns the
// raw client-go reflector "forbidden" spam into one clear, actionable line.
package startupcheck

import (
	"context"
	"log/slog"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Target is a resource the manager watches. Group/Resource address the
// SelfSubjectAccessReview; GVK resolves the CRD through the RESTMapper.
type Target struct {
	GVK      schema.GroupVersionKind
	Group    string
	Resource string
}

// Problem is an unmet watch prerequisite.
type Problem struct {
	Target Target
	Reason string
}

// AccessReviewer creates SelfSubjectAccessReviews — satisfied by
// client-go's authorizationv1 SelfSubjectAccessReviewInterface.
type AccessReviewer interface {
	Create(ctx context.Context, sar *authorizationv1.SelfSubjectAccessReview, opts metav1.CreateOptions) (*authorizationv1.SelfSubjectAccessReview, error)
}

// Checker verifies watch prerequisites against the live cluster.
type Checker struct {
	Mapper apimeta.RESTMapper
	Review AccessReviewer
	Logger *slog.Logger
}

// verbs the operator needs on each watched resource to run its informer.
var requiredVerbs = []string{"list", "watch"}

// Check returns the unmet prerequisites among targets: a CRD the RESTMapper
// cannot resolve (not installed) or a resource the operator SA may not
// list/watch (RBAC). An access-review API error is reported rather than assumed
// allowed, so a transient failure surfaces instead of hiding a real gap.
func (c *Checker) Check(ctx context.Context, targets []Target) []Problem {
	// A DeferredDiscoveryRESTMapper caches misses, so a CRD installed after boot
	// stays invisible until the cache is dropped.
	if resetter, ok := c.Mapper.(interface{ Reset() }); ok {
		resetter.Reset()
	}
	var problems []Problem
	for _, t := range targets {
		if _, err := c.Mapper.RESTMapping(t.GVK.GroupKind(), t.GVK.Version); err != nil {
			problems = append(problems, Problem{Target: t, Reason: "CRD is not installed"})
			continue
		}
		if reason := c.reviewAccess(ctx, t); reason != "" {
			problems = append(problems, Problem{Target: t, Reason: reason})
		}
	}
	return problems
}

func (c *Checker) reviewAccess(ctx context.Context, t Target) string {
	for _, verb := range requiredVerbs {
		review := &authorizationv1.SelfSubjectAccessReview{
			Spec: authorizationv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Group:    t.Group,
					Resource: t.Resource,
					Verb:     verb,
				},
			},
		}
		resp, err := c.Review.Create(ctx, review, metav1.CreateOptions{})
		switch {
		case apierrors.IsNotFound(err):
			// The authorization API itself is unavailable — cannot judge; skip.
			return ""
		case err != nil:
			return "cannot check RBAC (" + verb + "): " + err.Error()
		case !resp.Status.Allowed:
			return "operator ServiceAccount may not " + verb + " it — grant list+watch in the operator ClusterRole"
		}
	}
	return ""
}

// LogUntilReady polls Check every interval, logging one clear WARN per unmet
// prerequisite, until all are met (or ctx is cancelled). It returns without
// error either way — it is diagnostic, never a gate. The first all-clear pass
// logs an INFO so the recovery is visible in the log.
func (c *Checker) LogUntilReady(ctx context.Context, targets []Target, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	warned := false
	for {
		problems := c.Check(ctx, targets)
		if len(problems) == 0 {
			if warned {
				c.Logger.Info("all watched resources are now installed and permitted; reconciliation can proceed")
			}
			return
		}
		warned = true
		for _, p := range problems {
			c.Logger.Warn("waiting for a watched resource before reconciling",
				"resource", p.Target.Resource+"."+p.Target.Group,
				"reason", p.Reason)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
