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
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// testGuardDelay is a small but non-zero GuardDelay so renewer tests
// exercise the dual-CA path without sleeping multi-seconds. The 5s
// production default is excessive in-process where certwatcher
// reload latency is fictional.
const testGuardDelay = 1 * time.Millisecond

func TestRenewer_RenewOnce_WritesFreshCertAndPatchesVWC(t *testing.T) {
	dir := t.TempDir()
	vwc := makeVWC("vjsonnet", 1, nil)
	fake := &fakeVWCClient{vwc: vwc}

	r := &Renewer{
		Input:      Input{Namespace: "jaas-system"},
		CertDir:    dir,
		VWCName:    "vjsonnet",
		VWCClient:  fake,
		GuardDelay: testGuardDelay,
	}
	if err := r.renewOnce(context.Background()); err != nil {
		t.Fatalf("renewOnce: %v", err)
	}

	for _, want := range []string{"tls.crt", "tls.key"} {
		info, err := os.Stat(filepath.Join(dir, want))
		if err != nil {
			t.Errorf("missing %s: %v", want, err)
		} else if info.Size() == 0 {
			t.Errorf("%s is empty", want)
		}
	}
	if fake.updates == 0 {
		t.Error("VWC patch was not issued")
	}
}

// TestRenewer_RenewOnce_DualCARotation pins the dual-CA invariant:
// the union patch (caBundle = OldCA + NewCA) lands BEFORE WriteTo,
// and the trim patch (caBundle drops this pod's OldCA) lands AFTER
// WriteTo. Every admission request in flight during the rotation
// verifies under at least one trusted CA — admission failure is bounded
// by the apiserver's own retry, not by our rotation window.
func TestRenewer_RenewOnce_DualCARotation(t *testing.T) {
	dir := t.TempDir()
	oldBundle, err := Generate(Input{Namespace: "jaas-system"})
	if err != nil {
		t.Fatalf("generate old CA: %v", err)
	}
	oldCA := oldBundle.CABundle
	fake := &recordingVWCClient{
		fakeVWCClient: fakeVWCClient{vwc: makeVWC("vjsonnet", 1, oldCA)},
		certPath:      filepath.Join(dir, "tls.crt"),
	}

	r := &Renewer{
		Input:      Input{Namespace: "jaas-system"},
		CertDir:    dir,
		VWCName:    "vjsonnet",
		VWCClient:  fake,
		GuardDelay: testGuardDelay,
		CurrentCA:  oldCA, // this pod's current CA — the trim drops exactly this
	}
	if err := r.renewOnce(context.Background()); err != nil {
		t.Fatalf("renewOnce: %v", err)
	}

	if got := len(fake.patches); got != 2 {
		t.Fatalf("got %d patches, want 2 (union, then trim)", got)
	}
	// Patch 1: union — must contain OldCA, must land before WriteTo.
	if !bytes.Contains(fake.patches[0].caBundle, oldCA) {
		t.Errorf("first patch missing OldCA")
	}
	if fake.patches[0].certExisted {
		t.Error("first patch landed AFTER WriteTo; union must come first")
	}
	// Patch 2: trim — must NOT contain OldCA, must land after WriteTo.
	if bytes.Contains(fake.patches[1].caBundle, oldCA) {
		t.Errorf("trim patch still carries this pod's OldCA")
	}
	if !fake.patches[1].certExisted {
		t.Error("trim patch landed BEFORE WriteTo; cert must be on disk before we drop trust for OldCert")
	}
	if bytes.Equal(r.CurrentCA, oldCA) {
		t.Error("CurrentCA not advanced to the freshly generated CA after rotation")
	}
}

// TestRenewer_RenewOnce_PreservesPeerCA pins the multi-replica
// invariant: when the VWC already trusts another replica's CA, a
// rotation must drop only THIS pod's superseded CA, never the peer's —
// otherwise the peer's admission breaks until its own next rotation.
func TestRenewer_RenewOnce_PreservesPeerCA(t *testing.T) {
	dir := t.TempDir()
	mine, err := Generate(Input{Namespace: "jaas-system"})
	if err != nil {
		t.Fatalf("generate mine: %v", err)
	}
	peer, err := Generate(Input{Namespace: "jaas-system"})
	if err != nil {
		t.Fatalf("generate peer: %v", err)
	}
	// The VWC already trusts this pod's CA and a peer replica's CA.
	seeded := append(append([]byte{}, mine.CABundle...), peer.CABundle...)
	fake := &recordingVWCClient{
		fakeVWCClient: fakeVWCClient{vwc: makeVWC("vjsonnet", 1, seeded)},
		certPath:      filepath.Join(dir, "tls.crt"),
	}
	r := &Renewer{
		Input:      Input{Namespace: "jaas-system"},
		CertDir:    dir,
		VWCName:    "vjsonnet",
		VWCClient:  fake,
		GuardDelay: testGuardDelay,
		CurrentCA:  mine.CABundle,
	}
	if err := r.renewOnce(context.Background()); err != nil {
		t.Fatalf("renewOnce: %v", err)
	}
	trim := fake.patches[len(fake.patches)-1].caBundle
	if !bytes.Contains(trim, peer.CABundle) {
		t.Error("trim evicted the peer replica's CA; multi-replica admission would break")
	}
	if bytes.Contains(trim, mine.CABundle) {
		t.Error("trim kept this pod's superseded CA; should have dropped it")
	}
}

// recordingVWCClient observes every Update: the caBundle being written
// and whether tls.crt existed on disk at that moment.
type recordingVWCClient struct {
	fakeVWCClient
	certPath string
	patches  []recordedPatch
}

type recordedPatch struct {
	caBundle    []byte
	certExisted bool
}

func (o *recordingVWCClient) Update(ctx context.Context, vwc *admissionv1.ValidatingWebhookConfiguration, opts metav1.UpdateOptions) (*admissionv1.ValidatingWebhookConfiguration, error) {
	_, statErr := os.Stat(o.certPath)
	o.patches = append(o.patches, recordedPatch{
		caBundle:    append([]byte(nil), vwc.Webhooks[0].ClientConfig.CABundle...),
		certExisted: statErr == nil,
	})
	return o.fakeVWCClient.Update(ctx, vwc, opts)
}

// mergeCABundle must drop expired CA blocks (so the bundle stays bounded
// across many rotations), keep valid ones, and deduplicate.
func TestMergeCABundle_PrunesExpiredAndDedups(t *testing.T) {
	valid, err := Generate(Input{Namespace: "x", Validity: time.Hour})
	if err != nil {
		t.Fatalf("generate valid: %v", err)
	}
	expired, err := Generate(Input{Namespace: "x", NotBefore: time.Now().Add(-48 * time.Hour), Validity: time.Hour})
	if err != nil {
		t.Fatalf("generate expired: %v", err)
	}
	// current carries valid, the expired CA, and a duplicate of valid.
	current := bytes.Join([][]byte{valid.CABundle, expired.CABundle, valid.CABundle}, nil)
	out := mergeCABundle(current, nil, nil, time.Now())
	if !bytes.Contains(out, valid.CABundle) {
		t.Error("dropped a still-valid CA")
	}
	if bytes.Contains(out, expired.CABundle) {
		t.Error("kept an expired CA; bundle would grow unbounded")
	}
	if c := bytes.Count(out, valid.CABundle); c != 1 {
		t.Errorf("valid CA appears %d times, want 1 (dedup)", c)
	}
}

// certExpired treats an unreadable CA block as NOT expired — dropping a CA
// we can't parse would be more dangerous than keeping it. Pin that choice.
func TestCertExpired_UnparseableBlockTreatedAsNotExpired(t *testing.T) {
	if certExpired([]byte("not a pem block"), time.Now()) {
		t.Error("garbage bytes should be treated as not-expired")
	}
	junk := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not-a-cert")})
	if certExpired(junk, time.Now()) {
		t.Error("a PEM block that isn't a parseable cert should be treated as not-expired")
	}
}

func TestRenewer_RenewOnce_ProducesNewSerialEachCall(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeVWCClient{vwc: makeVWC("vjsonnet", 1, nil)}
	r := &Renewer{
		Input:      Input{Namespace: "jaas-system"},
		CertDir:    dir,
		VWCName:    "vjsonnet",
		VWCClient:  fake,
		GuardDelay: testGuardDelay,
	}
	if err := r.renewOnce(context.Background()); err != nil {
		t.Fatalf("first renewOnce: %v", err)
	}
	firstSerial := readSerial(t, filepath.Join(dir, "tls.crt"))

	if err := r.renewOnce(context.Background()); err != nil {
		t.Fatalf("second renewOnce: %v", err)
	}
	secondSerial := readSerial(t, filepath.Join(dir, "tls.crt"))

	if firstSerial.Cmp(secondSerial) == 0 {
		t.Errorf("renewal produced identical serial %v — cert was not regenerated", firstSerial)
	}
}

func TestRenewer_Run_TicksAtConfiguredInterval(t *testing.T) {
	dir := t.TempDir()
	// Seed the VWC with a pre-existing matching CA so the first tick
	// is forced to produce *new* bytes; otherwise the idempotent
	// short-circuit in PatchVWCCABundle would zero our patch counter.
	fake := &fakeVWCClient{vwc: makeVWC("vjsonnet", 1, nil)}
	r := &Renewer{
		Input:      Input{Namespace: "jaas-system"},
		CertDir:    dir,
		VWCName:    "vjsonnet",
		VWCClient:  fake,
		Interval:   20 * time.Millisecond,
		GuardDelay: testGuardDelay,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	// Sleep just past the deadline so the goroutine has had ~4 ticks.
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil after ctx cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after ctx cancel")
	}

	if fake.updates == 0 {
		t.Error("no rotation occurred during 100ms / 20ms ticks")
	}
}

func TestRenewer_Run_RejectsEmptyConfig(t *testing.T) {
	cases := []struct {
		name string
		r    *Renewer
	}{
		{
			name: "missing CertDir",
			r:    &Renewer{VWCName: "x", VWCClient: &fakeVWCClient{}},
		},
		{
			name: "missing VWCName",
			r:    &Renewer{CertDir: t.TempDir(), VWCClient: &fakeVWCClient{}},
		},
		{
			name: "missing VWCClient",
			r:    &Renewer{CertDir: t.TempDir(), VWCName: "x"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.r.Run(context.Background())
			if err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

func TestRenewer_Run_DefaultIntervalIsValidityOverThree(t *testing.T) {
	// Drive a single tick at the default-derived interval by setting
	// a very short validity (60ms / 3 = 20ms interval).
	dir := t.TempDir()
	fake := &fakeVWCClient{vwc: makeVWC("vjsonnet", 1, nil)}
	r := &Renewer{
		Input:      Input{Namespace: "jaas-system", Validity: 60 * time.Millisecond},
		CertDir:    dir,
		VWCName:    "vjsonnet",
		VWCClient:  fake,
		GuardDelay: testGuardDelay,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	<-done
	if fake.updates == 0 {
		t.Error("no rotation occurred at default-derived interval")
	}
}

func TestRenewer_Run_RenewalFailureDoesNotTerminateLoop(t *testing.T) {
	dir := t.TempDir()
	// Use a Patch client that errors on the first invocation and
	// succeeds afterwards; the loop must keep ticking past the failure.
	pc := &flakyUpdateClient{vwc: makeVWC("vjsonnet", 1, nil)}
	r := &Renewer{
		Input:      Input{Namespace: "jaas-system"},
		CertDir:    dir,
		VWCName:    "vjsonnet",
		VWCClient:  pc,
		Interval:   10 * time.Millisecond,
		GuardDelay: testGuardDelay,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if err := r.Run(ctx); err != nil {
		t.Errorf("Run returned %v after ctx cancel, want nil", err)
	}
	if pc.calls < 2 {
		t.Errorf("only %d renewal attempts in 60ms — loop bailed on first error?", pc.calls)
	}
}

// TestRenewer_Run_ShutdownDuringGuardDelayIsNotAFailure pins that a SIGTERM
// (ctx cancel) landing during renewOnce's guard-delay makes renewOnce return
// context.Canceled — a clean shutdown, not a rotation failure. The loop must
// NOT fire OnFailure (which would inflate the renewal-failures counter) nor
// log it as a failure; it just returns nil.
func TestRenewer_Run_ShutdownDuringGuardDelayIsNotAFailure(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeVWCClient{vwc: makeVWC("vjsonnet", 1, nil)}
	var onFailureCalls int
	r := &Renewer{
		Input:     Input{Namespace: "jaas-system"},
		CertDir:   dir,
		VWCName:   "vjsonnet",
		VWCClient: fake,
		Interval:  10 * time.Millisecond,
		// Long guard delay so the first tick is parked in the post-write
		// sleep when ctx is cancelled, forcing renewOnce → context.Canceled.
		GuardDelay: 2 * time.Second,
		OnFailure:  func(error) { onFailureCalls++ },
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	// Let the first tick fire and enter the guard delay, then cancel.
	time.Sleep(60 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil on shutdown-during-guard", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit promptly after ctx cancel during guard delay")
	}
	if onFailureCalls != 0 {
		t.Errorf("OnFailure fired %d times for a clean shutdown; context.Canceled must not count as a failure", onFailureCalls)
	}
}

// flakyUpdateClient is the minimum VWCClient surface the renewer test
// needs, with an Update that errors on the first call and succeeds on
// every call after. Lets the renewer test prove its loop survives a
// renew failure. The first-call error is generic (non-retriable) so
// UpdateVWCCABundle surfaces it immediately rather than retrying.
type flakyUpdateClient struct {
	vwc   *admissionv1.ValidatingWebhookConfiguration
	calls int
}

func (f *flakyUpdateClient) Get(ctx context.Context, name string, opts metav1.GetOptions) (*admissionv1.ValidatingWebhookConfiguration, error) {
	return f.vwc.DeepCopy(), nil
}

func (f *flakyUpdateClient) Update(ctx context.Context, vwc *admissionv1.ValidatingWebhookConfiguration, opts metav1.UpdateOptions) (*admissionv1.ValidatingWebhookConfiguration, error) {
	f.calls++
	if f.calls == 1 {
		return nil, errors.New("first-call failure")
	}
	f.vwc = vwc.DeepCopy()
	return f.vwc, nil
}

func readSerial(t *testing.T, path string) *big.Int {
	t.Helper()
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		t.Fatal("PEM decode failed")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	return cert.SerialNumber
}
