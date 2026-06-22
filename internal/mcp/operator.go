/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package mcp

import (
	"context"
	"fmt"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	jaasv1 "github.com/metio/jaas/api/v1"
)

// snippetSummary is the per-snippet row returned by list_snippets — enough to
// triage at a glance without fetching each one.
type snippetSummary struct {
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
	Ready       string `json:"ready" jsonschema:"the Ready condition status: True, False, or Unknown"`
	Reason      string `json:"reason,omitempty" jsonschema:"the Ready condition reason (a wire-stable code)"`
	Suspended   bool   `json:"suspended"`
	Revision    string `json:"revision,omitempty" jsonschema:"the last successfully reconciled source revision (sha256)"`
	ArtifactURL string `json:"artifactURL,omitempty" jsonschema:"HTTP URL of the last published artifact tarball"`
}

// snippetDetail is the full per-snippet view returned by get_snippet.
type snippetDetail struct {
	Namespace          string           `json:"namespace"`
	Name               string           `json:"name"`
	Ready              string           `json:"ready" jsonschema:"the Ready condition status: True, False, or Unknown"`
	Reason             string           `json:"reason,omitempty" jsonschema:"the Ready condition reason (a wire-stable code)"`
	Message            string           `json:"message,omitempty" jsonschema:"the Ready condition human-readable message"`
	RunbookURL         string           `json:"runbookURL,omitempty" jsonschema:"the per-reason remediation page for the current reason"`
	Suspended          bool             `json:"suspended"`
	Revision           string           `json:"revision,omitempty"`
	ArtifactURL        string           `json:"artifactURL,omitempty"`
	ObservedGeneration int64            `json:"observedGeneration"`
	LastSyncTime       string           `json:"lastSyncTime,omitempty" jsonschema:"RFC3339 timestamp of the last successful reconcile"`
	History            []revisionRecord `json:"history,omitempty" jsonschema:"retained revisions, most recent first; downstream consumers can pin an older sha256"`
}

type revisionRecord struct {
	Revision string `json:"revision"`
	Time     string `json:"time" jsonschema:"RFC3339 timestamp this revision was published"`
}

type listSnippetsInput struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"namespace to list; empty lists JsonnetSnippets across all namespaces the server can read"`
}

type listSnippetsOutput struct {
	Snippets []snippetSummary `json:"snippets"`
}

type getSnippetInput struct {
	Namespace string `json:"namespace" jsonschema:"the snippet's namespace"`
	Name      string `json:"name" jsonschema:"the snippet's name"`
}

// registerOperatorTools wires the in-cluster read tools. It is only called when
// the server is configured with a Kubernetes client (the embedded, operator-mode
// deployment).
func registerOperatorTools(server *mcpsdk.Server, cfg Config) {
	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name:        "list_snippets",
		Description: "List JsonnetSnippet resources with their Ready status, reason, suspend state, revision, and artifact URL. Omit namespace to list across all namespaces the operator can read.",
	}, cfg.listSnippetsHandler)

	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name:        "get_snippet",
		Description: "Get one JsonnetSnippet's full status: Ready condition (status, reason, message), the per-reason runbook URL, suspend state, revision, artifact URL, and the retained revision history.",
	}, cfg.getSnippetHandler)

	// diff_revisions reads published artifacts in-process, so it needs the
	// artifact backend in addition to the Kubernetes client. It stays read-only.
	if cfg.Store != nil {
		mcpsdk.AddTool(server, &mcpsdk.Tool{
			Name:        "diff_revisions",
			Description: "Diff the published output of a JsonnetSnippet between two retained revisions. Omit the revisions to compare the two most recent in status.history. Returns a per-file unified diff of the artifact contents.",
		}, cfg.diffRevisionsHandler)
	}
}

func (cfg Config) listSnippetsHandler(ctx context.Context, _ *mcpsdk.CallToolRequest, in listSnippetsInput) (*mcpsdk.CallToolResult, listSnippetsOutput, error) {
	var list jaasv1.JsonnetSnippetList
	var opts []client.ListOption
	if in.Namespace != "" {
		opts = append(opts, client.InNamespace(in.Namespace))
	}
	if err := cfg.KubeClient.List(ctx, &list, opts...); err != nil {
		// A cluster-wide list under a namespace-scoped operator SA (the
		// --watch-namespaces install) is a single Forbidden for the whole call,
		// not a partial result — hint that an explicit namespace would succeed.
		if in.Namespace == "" && apierrors.IsForbidden(err) {
			return errorResult(fmt.Sprintf("cannot list JsonnetSnippets cluster-wide: %v; this operator may be namespace-scoped — pass an explicit namespace", err)), listSnippetsOutput{}, nil
		}
		return errorResult(fmt.Sprintf("cannot list JsonnetSnippets: %v", err)), listSnippetsOutput{}, nil
	}
	out := listSnippetsOutput{Snippets: make([]snippetSummary, 0, len(list.Items))}
	for i := range list.Items {
		s := &list.Items[i]
		ready, reason, _ := readyCondition(s)
		out.Snippets = append(out.Snippets, snippetSummary{
			Namespace:   s.Namespace,
			Name:        s.Name,
			Ready:       ready,
			Reason:      reason,
			Suspended:   s.Spec.Suspend,
			Revision:    s.Status.Revision,
			ArtifactURL: s.Status.ArtifactURL,
		})
	}
	return nil, out, nil
}

func (cfg Config) getSnippetHandler(ctx context.Context, _ *mcpsdk.CallToolRequest, in getSnippetInput) (*mcpsdk.CallToolResult, snippetDetail, error) {
	if in.Namespace == "" || in.Name == "" {
		return errorResult("both namespace and name are required"), snippetDetail{}, nil
	}
	var snip jaasv1.JsonnetSnippet
	key := client.ObjectKey{Namespace: in.Namespace, Name: in.Name}
	if err := cfg.KubeClient.Get(ctx, key, &snip); err != nil {
		return errorResult(fmt.Sprintf("cannot get JsonnetSnippet %s/%s: %v", in.Namespace, in.Name, err)), snippetDetail{}, nil
	}

	ready, reason, message := readyCondition(&snip)
	detail := snippetDetail{
		Namespace:          snip.Namespace,
		Name:               snip.Name,
		Ready:              ready,
		Reason:             reason,
		Message:            message,
		RunbookURL:         cfg.runbookURL(reason),
		Suspended:          snip.Spec.Suspend,
		Revision:           snip.Status.Revision,
		ArtifactURL:        snip.Status.ArtifactURL,
		ObservedGeneration: snip.Status.ObservedGeneration,
	}
	if t := snip.Status.LastSyncTime; t != nil {
		detail.LastSyncTime = t.UTC().Format(rfc3339)
	}
	for _, h := range snip.Status.History {
		detail.History = append(detail.History, revisionRecord{
			Revision: h.Revision,
			Time:     h.Time.UTC().Format(rfc3339),
		})
	}
	return nil, detail, nil
}

// readyCondition extracts the Ready condition's status/reason/message. A
// snippet with no Ready condition yet (just created) reports status "Unknown"
// with empty reason and message.
func readyCondition(snip *jaasv1.JsonnetSnippet) (status, reason, message string) {
	cond := apimeta.FindStatusCondition(snip.Status.Conditions, jaasv1.ConditionReady)
	if cond == nil {
		return string(metav1.ConditionUnknown), "", ""
	}
	return string(cond.Status), cond.Reason, cond.Message
}

// runbookURL builds the per-reason remediation page link, matching the
// operator's own decorateMessage convention (base + lower(reason) + "/"). It
// returns "" when there's no reason or no configured base URL.
func (cfg Config) runbookURL(reason string) string {
	if reason == "" || cfg.RunbookBaseURL == "" {
		return ""
	}
	return cfg.RunbookBaseURL + strings.ToLower(reason) + "/"
}
