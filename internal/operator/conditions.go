/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

// Wire-stable Reason strings on the Ready status condition. Programmatic
// callers and runbooks key off these values; renaming them is a breaking
// change.
const (
	// ReasonPending marks a snippet that has been observed but not yet
	// reconciled — primarily transient, between watch event and first
	// completed reconcile pass.
	ReasonPending = "Pending"

	// ReasonSynced is set on Ready=True when the most recent reconcile
	// completed end-to-end and produced a publishable artifact.
	ReasonSynced = "Synced"

	// ReasonInvalidSpec covers spec-level validation failures that
	// admission should have caught: missing main.jsonnet, both/neither of
	// files/sourceRef set, etc.
	ReasonInvalidSpec = "InvalidSpec"

	// ReasonLibraryNotFound is set when a spec.libraries entry references
	// a JsonnetLibrary CR that the controller cannot Get (deleted, wrong
	// name, wrong namespace).
	ReasonLibraryNotFound = "LibraryNotFound"

	// ReasonCrossNamespaceRefRejected fires when --no-cross-namespace-refs
	// is enabled and a snippet references a library or source outside its
	// own namespace.
	ReasonCrossNamespaceRefRejected = "CrossNamespaceRefRejected"

	// ReasonExternalVariableConflict fires when a CR's
	// spec.externalVariables names a key the operator already owns via
	// --ext-var.
	ReasonExternalVariableConflict = "ExternalVariableConflict"

	// ReasonServiceAccountMissing fires when neither spec.serviceAccountName
	// nor --default-service-account is set.
	ReasonServiceAccountMissing = "ServiceAccountMissing"

	// ReasonEvaluationFailed wraps any go-jsonnet diagnostic surfaced from
	// the eval package — syntax error, runtime error, etc.
	ReasonEvaluationFailed = "EvaluationFailed"

	// ReasonEvaluationTimeout fires when the eval-package deadline was
	// reached before the snippet finished.
	ReasonEvaluationTimeout = "EvaluationTimeout"

	// ReasonSourceRefNotYetSupported is set when a CR declares
	// spec.sourceRef but the operator was built without a Fetcher (tests
	// and the bare-bones binary path). Production wiring always installs
	// the Fetcher; users seeing this in real clusters indicate a
	// mis-deployed operator.
	ReasonSourceRefNotYetSupported = "SourceRefNotYetSupported"

	// ReasonSourceNotReady fires when the referenced Flux source CR
	// exists but its status.conditions[Ready] is not True yet, or its
	// status.artifact is unpopulated. Re-reconcile when the source flips
	// to Ready — controller-runtime currently doesn't watch source CRs so
	// the user may need to nudge the snippet to retrigger.
	ReasonSourceNotReady = "SourceNotReady"

	// ReasonSourceFetchFailed fires when the Fetcher can't materialise
	// the artifact: HTTP failure, digest mismatch, tar corruption, or any
	// other error not classified as "not ready".
	ReasonSourceFetchFailed = "SourceFetchFailed"

	// ReasonDependencyCycle fires when a snippet's spec.sourceRef chain
	// transitively points back at itself — directly (A → EA(A)) or
	// through other snippets (A → EA(B) → EA(A)). The reconciler refuses
	// to publish so chained snippets don't loop forever; the webhook
	// rejects new CRs that introduce the cycle at admission.
	ReasonDependencyCycle = "DependencyCycle"

	// ReasonArtifactTooLarge fires when a snippet's rendered content
	// exceeds Publisher.MaxArtifactBytes. Stops one runaway snippet
	// from filling the storage volume; operators tune the bound via
	// --max-artifact-bytes.
	ReasonArtifactTooLarge = "ArtifactTooLarge"

	// ReasonSuspended is set on Ready=False whenever spec.suspend is
	// true. The previous Status.Revision and the on-disk artifact are
	// preserved so downstream Flux consumers continue serving the last
	// published bytes — suspending is "pause writes", not "rollback".
	ReasonSuspended = "Suspended"

	// ReasonRBACDenied is set on Ready=False when an apiserver call
	// fails with Forbidden, or a source CR's kind is not registered
	// with the apiserver. Both cases are non-recoverable by retry —
	// the cluster operator has to grant the missing verb (Forbidden)
	// or install the missing CRD (NoMatchError). The reconciler stops
	// engaging backoff for these errors so the workqueue isn't
	// burning cycles on permanently-failing snippets.
	//
	// Distinct from ReasonSourceFetchFailed (network / 5xx / digest
	// mismatch — different remediation) and ReasonLibraryNotFound
	// (the library CR truly doesn't exist — different remediation).
	// The message always names the verb + resource the operator must
	// grant so kubectl describe sends them straight to the fix.
	ReasonRBACDenied = "RBACDenied"
)

// AllReasons enumerates every wire-stable Reason the reconciler can set
// on the Ready condition. The drift-gate test in conditions_test.go
// asserts every entry has a matching docs/runbooks/<reason>.md, so a
// new Reason cannot ship without a remediation page.
var AllReasons = []string{
	ReasonPending,
	ReasonSynced,
	ReasonInvalidSpec,
	ReasonLibraryNotFound,
	ReasonCrossNamespaceRefRejected,
	ReasonExternalVariableConflict,
	ReasonServiceAccountMissing,
	ReasonEvaluationFailed,
	ReasonEvaluationTimeout,
	ReasonSourceRefNotYetSupported,
	ReasonSourceNotReady,
	ReasonSourceFetchFailed,
	ReasonDependencyCycle,
	ReasonArtifactTooLarge,
	ReasonSuspended,
	ReasonRBACDenied,
}

// FinalizerName is set on every JsonnetSnippet under management so the
// reconciler can clean up its published ExternalArtifact before the API
// removes the object. The string is part of the on-disk contract: changing
// it orphans finalizers on existing snippets and blocks their deletion.
const FinalizerName = "jaas.metio.wtf/finalizer"
