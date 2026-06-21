/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package mcp

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pmezard/go-difflib/difflib"
	"sigs.k8s.io/controller-runtime/pkg/client"

	jaasv1 "github.com/metio/jaas/api/v1"
	"github.com/metio/jaas/internal/storage"
)

// maxDiffArtifactBytes caps the decompressed size of each revision's artifact
// the diff tool holds in memory. The artifacts are the operator's own rendered
// output (it wrote them), so this is a heap guard against a pathologically large
// snippet, not an untrusted-input defense.
const maxDiffArtifactBytes = 32 << 20 // 32 MiB

type diffRevisionsInput struct {
	Namespace string `json:"namespace" jsonschema:"the snippet's namespace"`
	Name      string `json:"name" jsonschema:"the snippet's name"`
	From      string `json:"from,omitempty" jsonschema:"earlier revision (sha256); defaults to the second-most-recent retained revision in status.history"`
	To        string `json:"to,omitempty" jsonschema:"later revision (sha256); defaults to the most recent retained revision in status.history"`
}

// fileDiff is one changed file between the two revisions.
type fileDiff struct {
	Path   string `json:"path"`
	Status string `json:"status" jsonschema:"added, removed, or modified"`
	Diff   string `json:"diff,omitempty" jsonschema:"unified diff of the file between the two revisions"`
}

type diffRevisionsOutput struct {
	Namespace string     `json:"namespace"`
	Name      string     `json:"name"`
	From      string     `json:"from"`
	To        string     `json:"to"`
	Files     []fileDiff `json:"files" jsonschema:"per-file changes; empty when the two revisions are byte-identical"`
	Unchanged int        `json:"unchanged" jsonschema:"count of files identical between the two revisions"`
}

func (cfg Config) diffRevisionsHandler(ctx context.Context, _ *mcpsdk.CallToolRequest, in diffRevisionsInput) (*mcpsdk.CallToolResult, diffRevisionsOutput, error) {
	if in.Namespace == "" || in.Name == "" {
		return errorResult("both namespace and name are required"), diffRevisionsOutput{}, nil
	}
	var snip jaasv1.JsonnetSnippet
	key := client.ObjectKey{Namespace: in.Namespace, Name: in.Name}
	if err := cfg.KubeClient.Get(ctx, key, &snip); err != nil {
		return errorResult(fmt.Sprintf("cannot get JsonnetSnippet %s/%s: %v", in.Namespace, in.Name, err)), diffRevisionsOutput{}, nil
	}

	from, to, err := resolveRevisions(&snip, in.From, in.To)
	if err != nil {
		return errorResult(err.Error()), diffRevisionsOutput{}, nil
	}

	fromFiles, err := cfg.readRevision(ctx, in.Namespace, in.Name, from)
	if err != nil {
		return errorResult(revisionReadError("from", from, err)), diffRevisionsOutput{}, nil
	}
	toFiles, err := cfg.readRevision(ctx, in.Namespace, in.Name, to)
	if err != nil {
		return errorResult(revisionReadError("to", to, err)), diffRevisionsOutput{}, nil
	}

	out := diffRevisionsOutput{Namespace: in.Namespace, Name: in.Name, From: from, To: to}
	for _, p := range unionPaths(fromFiles, toFiles) {
		a, inFrom := fromFiles[p]
		b, inTo := toFiles[p]
		switch {
		case inFrom && inTo && a == b:
			out.Unchanged++
		case inFrom && inTo:
			out.Files = append(out.Files, fileDiff{Path: p, Status: "modified", Diff: unifiedDiff(p, from, to, a, b)})
		case inTo:
			out.Files = append(out.Files, fileDiff{Path: p, Status: "added", Diff: unifiedDiff(p, from, to, "", b)})
		default:
			out.Files = append(out.Files, fileDiff{Path: p, Status: "removed", Diff: unifiedDiff(p, from, to, a, "")})
		}
	}
	return nil, out, nil
}

// resolveRevisions picks the revisions to compare. Explicit inputs win;
// each empty side defaults from status.history (most-recent first): `to` from
// the newest revision, `from` from the second-newest. The history depth needed
// depends on which side(s) must be defaulted — defaulting only `to` needs one
// retained revision, defaulting `from` needs two. A caller that supplies one
// side must not be told to "pass explicit from/to".
func resolveRevisions(snip *jaasv1.JsonnetSnippet, from, to string) (string, string, error) {
	hist := snip.Status.History
	if to == "" {
		if len(hist) < 1 {
			return "", "", fmt.Errorf("no retained revisions to diff for %s/%s; publish a revision first, or raise spec.history", snip.Namespace, snip.Name)
		}
		to = hist[0].Revision
	}
	if from == "" {
		if len(hist) < 2 {
			return "", "", fmt.Errorf("need two retained revisions to default 'from', but %s/%s has %d; pass an explicit from, or raise spec.history", snip.Namespace, snip.Name, len(hist))
		}
		from = hist[1].Revision
	}
	return from, to, nil
}

func (cfg Config) readRevision(ctx context.Context, namespace, name, revision string) (map[string]string, error) {
	rc, err := cfg.Store.Open(ctx, namespace, name, revision)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return extractTarGz(rc)
}

// extractTarGz reads a gzip'd tar into a path->content map, bounding the total
// in-memory size at maxDiffArtifactBytes.
func extractTarGz(r io.Reader) (map[string]string, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("gunzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	files := map[string]string{}
	var total int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("untar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		remaining := maxDiffArtifactBytes - total
		buf, err := io.ReadAll(io.LimitReader(tr, remaining+1))
		if err != nil {
			return nil, fmt.Errorf("read %q: %w", hdr.Name, err)
		}
		total += int64(len(buf))
		if total > maxDiffArtifactBytes {
			return nil, fmt.Errorf("artifact exceeds %d bytes; too large to diff", int64(maxDiffArtifactBytes))
		}
		// Defense-in-depth: the artifacts are operator-written, but never key the
		// diff map on a name that traverses, is absolute, or carries NUL/backslash
		// — matching the validation internal/sources applies on the fetch path, so
		// a future non-operator writer can't smuggle an escaping entry in here.
		clean := path.Clean(hdr.Name)
		if strings.ContainsRune(hdr.Name, 0) || strings.Contains(hdr.Name, `\`) ||
			clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
			continue
		}
		files[clean] = string(buf)
	}
	return files, nil
}

func unionPaths(a, b map[string]string) []string {
	set := make(map[string]struct{}, len(a)+len(b))
	for p := range a {
		set[p] = struct{}{}
	}
	for p := range b {
		set[p] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// unifiedDiff renders a unified diff of one file between the two revisions. The
// file labels carry the short revision so an agent reading the diff knows which
// side is which.
func unifiedDiff(path, fromRev, toRev, a, b string) string {
	text, _ := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:        difflib.SplitLines(a),
		B:        difflib.SplitLines(b),
		FromFile: fmt.Sprintf("%s (%s)", path, shortRev(fromRev)),
		ToFile:   fmt.Sprintf("%s (%s)", path, shortRev(toRev)),
		Context:  3,
	})
	return text
}

// shortRev trims the sha256: prefix and abbreviates to 12 hex chars for display.
func shortRev(rev string) string {
	h := strings.TrimPrefix(rev, "sha256:")
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func revisionReadError(which, rev string, err error) string {
	if errors.Is(err, storage.ErrRevisionNotFound) {
		return fmt.Sprintf("%s revision %s is not in the artifact store (never published, or pruned beyond spec.history)", which, shortRev(rev))
	}
	return fmt.Sprintf("cannot read %s revision %s: %v", which, shortRev(rev), err)
}
