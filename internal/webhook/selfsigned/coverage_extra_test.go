/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package selfsigned

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// WriteTo writes tls.key first, then tls.crt. When the cert write fails
// (here: a directory already occupies the tls.crt path) the second
// os.WriteFile errors and WriteTo surfaces it — the key half already
// landed on disk, but the call as a whole reports failure.
func TestBundle_WriteTo_CertWriteFailureSurfaces(t *testing.T) {
	dir := t.TempDir()
	// A directory at tls.crt makes the cert WriteFile fail while the
	// preceding tls.key write succeeds.
	if err := os.Mkdir(filepath.Join(dir, "tls.crt"), 0o700); err != nil {
		t.Fatalf("seed tls.crt dir: %v", err)
	}
	b, err := Generate(Input{Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.WriteTo(dir); err == nil {
		t.Error("expected error when tls.crt path is unwritable")
	}
	// The key write precedes the failing cert write, so tls.key is present.
	if _, err := os.Stat(filepath.Join(dir, "tls.key")); err != nil {
		t.Errorf("tls.key should have been written before the cert failure: %v", err)
	}
}

// WriteTo writes the key first; a directory occupying the tls.key path
// makes the very first write fail, so WriteTo reports the key error and
// never reaches the cert write.
func TestBundle_WriteTo_KeyWriteFailureSurfaces(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "tls.key"), 0o700); err != nil {
		t.Fatalf("seed tls.key dir: %v", err)
	}
	b, err := Generate(Input{Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	err = b.WriteTo(dir)
	if err == nil {
		t.Fatal("expected error when tls.key path is unwritable")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("tls.key")) {
		t.Errorf("error %v does not mention tls.key", err)
	}
}

// renewOnce's union patch runs before WriteTo. When that first
// UpdateVWCCABundle fails with a non-retriable error, renewOnce returns
// the wrapped "union" error and never writes the cert to disk.
func TestRenewer_RenewOnce_UnionPatchFailureSurfaces(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeVWCClient{
		vwc:       makeVWC("vjsonnet", 1, nil),
		updateErr: apierrors.NewForbidden(schema.GroupResource{Resource: "vwc"}, "vjsonnet", errors.New("nope")),
	}
	r := &Renewer{
		Input:      Input{Namespace: "jaas-system"},
		CertDir:    dir,
		VWCName:    "vjsonnet",
		VWCClient:  fake,
		GuardDelay: 1 * time.Millisecond,
	}
	err := r.renewOnce(context.Background())
	if err == nil {
		t.Fatal("expected renewOnce to fail when the union patch fails")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("union")) {
		t.Errorf("error %v not tagged as a union failure", err)
	}
	// The union patch is the first step; a failure there must abort
	// before the cert lands on disk.
	if _, statErr := os.Stat(filepath.Join(dir, "tls.crt")); statErr == nil {
		t.Error("tls.crt written despite union-patch failure; rotation should abort first")
	}
}

// renewOnce writes the cert after the union patch. Pointing CertDir at a
// non-existent directory makes WriteTo fail, so renewOnce returns the
// wrapped "write cert" error after the union patch already succeeded.
func TestRenewer_RenewOnce_WriteCertFailureSurfaces(t *testing.T) {
	fake := &fakeVWCClient{vwc: makeVWC("vjsonnet", 1, nil)}
	r := &Renewer{
		Input:      Input{Namespace: "jaas-system"},
		CertDir:    filepath.Join(t.TempDir(), "does-not-exist"),
		VWCName:    "vjsonnet",
		VWCClient:  fake,
		GuardDelay: 1 * time.Millisecond,
	}
	err := r.renewOnce(context.Background())
	if err == nil {
		t.Fatal("expected renewOnce to fail when the cert directory is missing")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("write cert")) {
		t.Errorf("error %v not tagged as a write-cert failure", err)
	}
	// The union patch precedes WriteTo, so the apiserver was touched once.
	if fake.updates != 1 {
		t.Errorf("Update called %d times, want 1 (union patch before the write failure)", fake.updates)
	}
}

// renewOnce's trim patch runs after WriteTo. A client that succeeds on
// the union patch but fails the trim makes renewOnce return the wrapped
// "trim" error — and CurrentCA must NOT have advanced, since the
// rotation did not complete.
func TestRenewer_RenewOnce_TrimPatchFailureSurfaces(t *testing.T) {
	dir := t.TempDir()
	old, err := Generate(Input{Namespace: "jaas-system"})
	if err != nil {
		t.Fatalf("generate old CA: %v", err)
	}
	oldCA := old.CABundle
	// Seed the VWC with this pod's old CA so the trim patch actually
	// diffs the bundle (drops oldCA) and issues a second Update — that
	// second Update is the one failOnNthUpdateClient rejects.
	fake := &failOnNthUpdateClient{
		vwc:    makeVWC("vjsonnet", 1, oldCA),
		failOn: 2, // union succeeds, trim fails
	}
	r := &Renewer{
		Input:      Input{Namespace: "jaas-system"},
		CertDir:    dir,
		VWCName:    "vjsonnet",
		VWCClient:  fake,
		GuardDelay: 1 * time.Millisecond,
		CurrentCA:  oldCA,
	}
	err = r.renewOnce(context.Background())
	if err == nil {
		t.Fatal("expected renewOnce to fail when the trim patch fails")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("trim")) {
		t.Errorf("error %v not tagged as a trim failure", err)
	}
	if !bytes.Equal(r.CurrentCA, oldCA) {
		t.Error("CurrentCA advanced despite the trim patch failing; rotation did not complete")
	}
}

// renewOnce honors a context cancelled during the guard delay: the
// post-write sleep selects on ctx.Done and returns ctx.Err() rather than
// proceeding to the trim patch.
func TestRenewer_RenewOnce_ContextCancelledDuringGuardDelay(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeVWCClient{vwc: makeVWC("vjsonnet", 1, nil)}
	r := &Renewer{
		Input:      Input{Namespace: "jaas-system"},
		CertDir:    dir,
		VWCName:    "vjsonnet",
		VWCClient:  fake,
		GuardDelay: 5 * time.Second, // long enough that the cancel wins the select
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: the guard-delay select fires ctx.Done immediately
	err := r.renewOnce(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("renewOnce error = %v, want context.Canceled", err)
	}
	// Only the union patch ran; the trim patch is after the guard delay.
	if fake.updates != 1 {
		t.Errorf("Update called %d times, want 1 (cancel landed in the guard delay)", fake.updates)
	}
}

// renewOnce falls back to the 5s production guard delay when GuardDelay
// is zero. Cancelling the context first makes the post-write select
// return immediately via ctx.Done, so the default delay never actually
// sleeps — this exercises the `guard <= 0` default branch without a
// multi-second wait.
func TestRenewer_RenewOnce_ZeroGuardDelayUsesDefault(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeVWCClient{vwc: makeVWC("vjsonnet", 1, nil)}
	r := &Renewer{
		Input:     Input{Namespace: "jaas-system"},
		CertDir:   dir,
		VWCName:   "vjsonnet",
		VWCClient: fake,
		// GuardDelay omitted → defaults to defaultGuardDelay.
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.renewOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("renewOnce error = %v, want context.Canceled", err)
	}
}

// renewOnce uses an injected clock when Renewer.now is set, instead of
// time.Now. A fixed past clock keeps every CA "not expired" during the
// merge, and the rotation completes normally.
func TestRenewer_RenewOnce_UsesInjectedClock(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeVWCClient{vwc: makeVWC("vjsonnet", 1, nil)}
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := &Renewer{
		Input:      Input{Namespace: "jaas-system"},
		CertDir:    dir,
		VWCName:    "vjsonnet",
		VWCClient:  fake,
		GuardDelay: 1 * time.Millisecond,
		now:        func() time.Time { return fixed },
	}
	if err := r.renewOnce(context.Background()); err != nil {
		t.Fatalf("renewOnce with injected clock: %v", err)
	}
	if fake.updates < 1 {
		t.Error("rotation did not patch the VWC with an injected clock")
	}
}

// pemCertBlocks must skip non-CERTIFICATE PEM blocks and stop at trailing
// garbage while still returning every real CERTIFICATE block, canonically
// re-encoded.
func TestPemCertBlocks_SkipsNonCertificateBlocksAndGarbage(t *testing.T) {
	bundle, err := Generate(Input{Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	// A private-key block (non-CERTIFICATE) sandwiched between two real
	// certs, followed by trailing non-PEM garbage.
	keyBlock := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("key-bytes")})
	input := bytes.Join([][]byte{
		keyBlock,
		bundle.CABundle,
		[]byte("trailing garbage that is not PEM\n"),
	}, nil)

	blocks := pemCertBlocks(input)
	if len(blocks) != 1 {
		t.Fatalf("got %d CERTIFICATE blocks, want 1 (the key block must be skipped)", len(blocks))
	}
	// The returned block must parse as the CA certificate.
	blk, _ := pem.Decode(blocks[0])
	if blk == nil {
		t.Fatal("returned block is not valid PEM")
	}
	if _, err := x509.ParseCertificate(blk.Bytes); err != nil {
		t.Errorf("returned block is not a parseable certificate: %v", err)
	}
}

// pemCertBlocks returns nil for input with no PEM blocks at all.
func TestPemCertBlocks_EmptyAndNonPEMInput(t *testing.T) {
	if got := pemCertBlocks(nil); got != nil {
		t.Errorf("nil input returned %d blocks, want nil", len(got))
	}
	if got := pemCertBlocks([]byte("definitely not pem")); got != nil {
		t.Errorf("non-PEM input returned %d blocks, want nil", len(got))
	}
}

// mergeCABundle without a current bundle returns just the add block — the
// single-replica bootstrap shape.
func TestMergeCABundle_EmptyCurrentReturnsAddOnly(t *testing.T) {
	bundle, err := Generate(Input{Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	out := mergeCABundle(nil, bundle.CABundle, nil, time.Now())
	if !bytes.Equal(out, bundle.CABundle) {
		t.Error("merging into an empty bundle should yield exactly the add block")
	}
}

// mergeCABundle drops a block listed in remove even when it is otherwise
// still valid — that is how a rotation evicts this pod's own superseded CA.
func TestMergeCABundle_RemovesNamedBlock(t *testing.T) {
	mine, err := Generate(Input{Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	peer, err := Generate(Input{Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	current := bytes.Join([][]byte{mine.CABundle, peer.CABundle}, nil)
	out := mergeCABundle(current, peer.CABundle, mine.CABundle, time.Now())
	if bytes.Contains(out, mine.CABundle) {
		t.Error("the removed block should not survive the merge")
	}
	if !bytes.Contains(out, peer.CABundle) {
		t.Error("the peer block should survive the merge")
	}
}

// CombineCABundles is the bootstrap union helper: a still-valid existing CA
// is preserved, an expired one pruned, and newCA always present.
func TestCombineCABundles_PreservesValidPrunesExpiredEnsuresNew(t *testing.T) {
	existing, err := Generate(Input{Namespace: "ns", Validity: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	expired, err := Generate(Input{Namespace: "ns", NotBefore: time.Now().Add(-48 * time.Hour), Validity: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	newCA, err := Generate(Input{Namespace: "ns", Validity: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	old := bytes.Join([][]byte{existing.CABundle, expired.CABundle}, nil)
	out := CombineCABundles(old, newCA.CABundle)
	if !bytes.Contains(out, existing.CABundle) {
		t.Error("CombineCABundles dropped a still-valid existing CA")
	}
	if bytes.Contains(out, expired.CABundle) {
		t.Error("CombineCABundles kept an expired CA")
	}
	if !bytes.Contains(out, newCA.CABundle) {
		t.Error("CombineCABundles did not ensure newCA is present")
	}
}

// failOnNthUpdateClient errors on its Nth Update call (1-indexed) and
// succeeds on every other call, so a test can fail the trim patch while
// letting the union patch through.
type failOnNthUpdateClient struct {
	vwc    *admissionv1.ValidatingWebhookConfiguration
	calls  int
	failOn int
}

func (f *failOnNthUpdateClient) Get(_ context.Context, _ string, _ metav1.GetOptions) (*admissionv1.ValidatingWebhookConfiguration, error) {
	return f.vwc.DeepCopy(), nil
}

func (f *failOnNthUpdateClient) Update(_ context.Context, vwc *admissionv1.ValidatingWebhookConfiguration, _ metav1.UpdateOptions) (*admissionv1.ValidatingWebhookConfiguration, error) {
	f.calls++
	if f.calls == f.failOn {
		return nil, errors.New("forced update failure")
	}
	f.vwc = vwc.DeepCopy()
	return f.vwc.DeepCopy(), nil
}
