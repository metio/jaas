/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package mcp

import (
	"context"
	"fmt"
	"time"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"sigs.k8s.io/controller-runtime/pkg/client"

	jaasv1 "github.com/metio/jaas/api/v1"
)

// mutateInput identifies the snippet a mutation acts on.
type mutateInput struct {
	Namespace string `json:"namespace" jsonschema:"the snippet's namespace"`
	Name      string `json:"name" jsonschema:"the snippet's name"`
}

type mutateOutput struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Result    string `json:"result" jsonschema:"a short description of what changed"`
}

// registerMutationTools wires the gated write tools. It is only called when the
// server has a Kubernetes client AND mutations are explicitly enabled
// (--mcp-allow-mutations), so a read-only deployment never exposes them.
func registerMutationTools(server *mcpsdk.Server, cfg Config) {
	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name:        "reconcile_snippet",
		Description: "Request an immediate reconcile of a JsonnetSnippet by stamping its reconcile.fluxcd.io/requestedAt annotation — the same trigger as `flux reconcile`.",
	}, cfg.reconcileSnippetHandler)

	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name:        "suspend_snippet",
		Description: "Pause reconciliation of a JsonnetSnippet by setting spec.suspend=true. The operator stops re-rendering it until it is resumed.",
	}, cfg.suspendSnippetHandler)

	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name:        "resume_snippet",
		Description: "Resume reconciliation of a suspended JsonnetSnippet by clearing spec.suspend.",
	}, cfg.resumeSnippetHandler)
}

func (cfg Config) reconcileSnippetHandler(ctx context.Context, _ *mcpsdk.CallToolRequest, in mutateInput) (*mcpsdk.CallToolResult, mutateOutput, error) {
	return cfg.mutateSnippet(ctx, in, func(s *jaasv1.JsonnetSnippet) (string, bool) {
		if s.Annotations == nil {
			s.Annotations = map[string]string{}
		}
		token := time.Now().UTC().Format(time.RFC3339Nano)
		s.Annotations[fluxmeta.ReconcileRequestAnnotation] = token
		return "reconcile requested at " + token, true
	})
}

func (cfg Config) suspendSnippetHandler(ctx context.Context, _ *mcpsdk.CallToolRequest, in mutateInput) (*mcpsdk.CallToolResult, mutateOutput, error) {
	return cfg.mutateSnippet(ctx, in, func(s *jaasv1.JsonnetSnippet) (string, bool) {
		if s.Spec.Suspend {
			return "already suspended", false
		}
		s.Spec.Suspend = true
		return "suspended", true
	})
}

func (cfg Config) resumeSnippetHandler(ctx context.Context, _ *mcpsdk.CallToolRequest, in mutateInput) (*mcpsdk.CallToolResult, mutateOutput, error) {
	return cfg.mutateSnippet(ctx, in, func(s *jaasv1.JsonnetSnippet) (string, bool) {
		if !s.Spec.Suspend {
			return "not suspended; no change", false
		}
		s.Spec.Suspend = false
		return "resumed", true
	})
}

// mutateSnippet Gets the snippet, applies mutate (which returns a result
// description and whether anything changed), and Patches only when something
// changed. The patch is a MergeFrom diff so concurrent status writes by the
// operator don't conflict with a spec/annotation change.
func (cfg Config) mutateSnippet(ctx context.Context, in mutateInput, mutate func(*jaasv1.JsonnetSnippet) (string, bool)) (*mcpsdk.CallToolResult, mutateOutput, error) {
	if in.Namespace == "" || in.Name == "" {
		return errorResult("both namespace and name are required"), mutateOutput{}, nil
	}
	var snip jaasv1.JsonnetSnippet
	key := client.ObjectKey{Namespace: in.Namespace, Name: in.Name}
	if err := cfg.KubeClient.Get(ctx, key, &snip); err != nil {
		return errorResult(fmt.Sprintf("cannot get JsonnetSnippet %s/%s: %v", in.Namespace, in.Name, err)), mutateOutput{}, nil
	}
	before := snip.DeepCopy()
	desc, changed := mutate(&snip)
	if changed {
		if err := cfg.KubeClient.Patch(ctx, &snip, client.MergeFrom(before)); err != nil {
			return errorResult(fmt.Sprintf("cannot update JsonnetSnippet %s/%s: %v", in.Namespace, in.Name, err)), mutateOutput{}, nil
		}
	}
	return nil, mutateOutput{Namespace: in.Namespace, Name: in.Name, Result: desc}, nil
}
