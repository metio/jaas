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
	"fmt"
	"log/slog"
	"time"
)

// defaultGuardDelay is how long renewOnce waits between writing the
// new cert and trimming the caBundle back to just NewCA. The window
// has to cover certwatcher's fsnotify reload (single-digit ms in
// practice) AND any in-flight admission requests using the old cert
// against the union-CA caBundle. 5s is generous on both — long
// enough that an apiserver round-trip in flight when WriteTo lands
// finishes well before the trim.
const defaultGuardDelay = 5 * time.Second

// Renewer periodically regenerates the self-signed cert + key, writes
// them to CertDir, and re-patches the named VWC's caBundle so the
// apiserver continues trusting the chain. Pairs with Generate's
// one-shot bootstrap to give the operator hot-reload-style rotation
// without cert-manager.
//
// File-change detection: controller-runtime's webhook server spawns
// sigs.k8s.io/controller-runtime/pkg/certwatcher internally when its
// CertDir is set, and certwatcher uses fsnotify to react to writes the
// instant they hit disk — no polling lag between renewal and
// activation. The Renewer therefore only needs to:
//
//  1. Generate a fresh bundle in-memory.
//  2. Write tls.crt + tls.key into CertDir.
//  3. Re-patch the VWC caBundle so the apiserver trusts the new CA.
//
// One Renewer per process; the manager runs it as a long-lived
// goroutine. Stops cleanly on ctx cancel.
type Renewer struct {
	// Input is the same shape Generate consumes — passed through on
	// every rotation so SANs / validity remain stable across renewals.
	Input Input

	// CertDir is where tls.crt and tls.key are written. Must match
	// the controller-runtime webhook server's CertDir.
	CertDir string

	// VWCName is the ValidatingWebhookConfiguration whose caBundle is
	// re-patched on each rotation.
	VWCName string

	// VWCClient is the apiserver client used to patch the VWC. Tests
	// substitute a fake; production wires the kubernetes clientset.
	VWCClient VWCClient

	// Interval is how often the renewer fires. Zero defaults to
	// Input.Validity / 3 (e.g. 30 days for a 90-day cert) — well
	// inside the kubelet/certwatcher refresh windows.
	Interval time.Duration

	// GuardDelay is how long renewOnce waits between writing the
	// new cert/key to CertDir and trimming the VWC caBundle back to
	// just the new CA. During this window the caBundle holds BOTH
	// OldCA and NewCA, so any admission request — whether the
	// webhook is still serving OldCert (pre-reload) or already
	// serving NewCert (post-reload) — verifies. Zero defaults to
	// defaultGuardDelay (5s). Tests typically set a small explicit
	// value (a few ms) to keep wall-clock short while still
	// exercising the dual-CA path.
	GuardDelay time.Duration

	// CurrentCA is this pod's CA PEM block — the one it currently serves
	// and whose trust it manages in the VWC caBundle. main.go seeds it
	// with the bootstrap CA; each rotation advances it to the freshly
	// generated CA. The trim step removes exactly this block (the pod's
	// *previous* CA) from the bundle, leaving every other replica's CA
	// untouched — so a rotation in a multi-replica install never evicts a
	// peer. Touched only by the single renewer goroutine after Run starts.
	CurrentCA []byte

	// Logger receives renewal events. nil falls back to slog.Default.
	Logger *slog.Logger

	// now supplies the wall-clock used to prune expired CA blocks from
	// the bundle. nil falls back to time.Now; tests inject a fixed clock.
	now func() time.Time

	// OnFailure, if non-nil, fires after every renewOnce error and
	// gets the error as its argument. main.go wires this to
	// operator.RecordWebhookCertRenewalFailure so the Prometheus
	// counter `jaas_webhook_cert_renewal_failures_total` tracks
	// background goroutine failures that would otherwise only surface as
	// slog.Warn lines.
	OnFailure func(error)
}

// Run blocks until ctx is canceled, regenerating + republishing the
// cert at every Interval tick. Returns the context's error on cancel
// (nil) so a SIGTERM produces a clean shutdown, not a CrashLoop signal.
//
// Run does NOT generate the initial cert — call Generate +
// UpdateVWCCABundle once at startup before mgr.Start, then hand off to
// Run. This separation keeps the boot sequence linear (no "is the cert
// ready yet?" race when the webhook server boots) and the renewal
// goroutine purely time-driven.
func (r *Renewer) Run(ctx context.Context) error {
	logger := r.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if r.CertDir == "" {
		return errors.New("renewer: CertDir is required")
	}
	if r.VWCName == "" {
		return errors.New("renewer: VWCName is required")
	}
	if r.VWCClient == nil {
		return errors.New("renewer: VWCClient is required")
	}

	interval := r.Interval
	if interval == 0 {
		validity := r.Input.Validity
		if validity == 0 {
			validity = 365 * 24 * time.Hour
		}
		interval = validity / 3
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			// Recover a panic in renewOnce so one bad tick can't kill
			// the goroutine and silently stop all future rotation — the
			// existing cert would then keep working until expiry, at
			// which point admission breaks cluster-wide with no prior
			// signal. A recovered panic is treated like any renew error.
			err := func() (err error) {
				defer func() {
					if p := recover(); p != nil {
						err = fmt.Errorf("renewer: panic in renewOnce: %v", p)
					}
				}()
				return r.renewOnce(ctx)
			}()
			if err != nil {
				// A SIGTERM during renewOnce's guard-delay cancels ctx and
				// renewOnce returns context.Canceled — that's a clean
				// shutdown, not a rotation failure. Skip the warn + OnFailure
				// so it doesn't spuriously log "renewal failed" and inflate
				// jaas_webhook_cert_renewal_failures_total; the next loop
				// iteration sees ctx.Done() and returns nil.
				if errors.Is(err, context.Canceled) {
					continue
				}
				// Surface as a warning rather than failing the whole
				// goroutine — the next tick gets another shot, and the
				// existing cert is still valid until its expiry. Fire
				// OnFailure so the operator-side Prometheus counter
				// ticks; without it the slog.Warn would be the only
				// signal of a silently-degrading rotation pipeline.
				logger.WarnContext(ctx, "Self-signed webhook cert renewal failed",
					slog.Any("error", err))
				if r.OnFailure != nil {
					r.OnFailure(err)
				}
				continue
			}
			logger.InfoContext(ctx, "Self-signed webhook cert renewed",
				slog.String("certDir", r.CertDir),
				slog.String("vwc", r.VWCName))
		}
	}
}

// renewOnce is the per-tick work — extracted so a test can drive a
// single rotation without standing up a ticker.
//
// Dual-CA, peer-preserving rotation sequence:
//
//  1. Generate the new (cert, key, CA).
//  2. Read the VWC's current caBundle — this pod's CurrentCA, plus any
//     other replicas' CAs and operator-supplied entries.
//  3. Patch the caBundle to (current ∪ NewCA), expired blocks pruned.
//     The apiserver now trusts the new chain alongside everything it
//     already trusted. The webhook is still serving OldCert.
//  4. WriteTo writes the new cert+key. fsnotify-driven certwatcher
//     reloads, the webhook switches to NewCert.
//  5. Sleep GuardDelay so any admission requests in flight against
//     OldCert finish under the union-trust caBundle, and certwatcher
//     completes its reload.
//  6. Re-read the caBundle (a peer may have rotated in the meantime) and
//     patch it to (current − thisPodOldCA + NewCA), expired blocks
//     pruned. Only this pod's *own* superseded CA is dropped; every
//     other replica's CA stays trusted, so a rotation never breaks a
//     peer's admission. Single-replica installs collapse this to "NewCA
//     only", identical to before.
//
// Every admission request during the sequence verifies under one of the
// trusted chains, so admission failures are bounded by the apiserver's
// own retry behavior, not by the rotation window.
func (r *Renewer) renewOnce(ctx context.Context) error {
	bundle, err := Generate(r.Input)
	if err != nil {
		return fmt.Errorf("renewer: generate: %w", err)
	}

	guard := r.GuardDelay
	if guard <= 0 {
		guard = defaultGuardDelay
	}
	now := time.Now
	if r.now != nil {
		now = r.now
	}

	// Union: add NewCA to whatever the VWC already trusts (peers' CAs
	// included), pruning expired blocks. UpdateVWCCABundle re-reads and
	// re-applies on conflict, so a peer rotating at the same time can't
	// clobber this write.
	if err := UpdateVWCCABundle(ctx, r.VWCClient, r.VWCName, func(cur []byte) []byte {
		return mergeCABundle(cur, bundle.CABundle, nil, now())
	}); err != nil {
		return fmt.Errorf("renewer: union: %w", err)
	}
	if err := bundle.WriteTo(r.CertDir); err != nil {
		return fmt.Errorf("renewer: write cert: %w", err)
	}
	select {
	case <-time.After(guard):
	case <-ctx.Done():
		return ctx.Err()
	}
	// Trim: drop only this pod's previous CA, keeping peers' CAs and the
	// new one. The mutate runs against the freshly-read bundle on every
	// (re)try, so it composes correctly with concurrent peer rotations.
	if err := UpdateVWCCABundle(ctx, r.VWCClient, r.VWCName, func(cur []byte) []byte {
		return mergeCABundle(cur, bundle.CABundle, r.CurrentCA, now())
	}); err != nil {
		return fmt.Errorf("renewer: trim: %w", err)
	}
	r.CurrentCA = bundle.CABundle
	return nil
}

// mergeCABundle returns a re-encoded CA bundle derived from current: every
// CERTIFICATE block is preserved EXCEPT ones that have expired (NotAfter
// not after now) or exactly equal a block in remove (this pod's superseded
// CA); add is then ensured present. Blocks are deduplicated and emitted in
// the canonical PEM form (each block newline-terminated), so the result is
// a valid multi-PEM caBundle. Preserving foreign blocks is what keeps a
// multi-replica rotation from evicting a peer; pruning expired blocks keeps
// the bundle bounded across many rotations.
func mergeCABundle(current, add, remove []byte, now time.Time) []byte {
	removeSet := map[string]bool{}
	for _, enc := range pemCertBlocks(remove) {
		removeSet[string(enc)] = true
	}
	seen := map[string]bool{}
	var out [][]byte
	for _, enc := range pemCertBlocks(current) {
		k := string(enc)
		if seen[k] || removeSet[k] {
			continue
		}
		if certExpired(enc, now) {
			continue
		}
		seen[k] = true
		out = append(out, enc)
	}
	// add is this pod's fresh CA: always keep it (deduped), never subject
	// to removeSet or the expiry check.
	for _, enc := range pemCertBlocks(add) {
		k := string(enc)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, enc)
	}
	return bytes.Join(out, nil)
}

// pemCertBlocks decodes data into its CERTIFICATE PEM blocks, each
// re-encoded canonically so equal certs compare byte-for-byte regardless
// of source whitespace. Non-CERTIFICATE blocks and trailing garbage are
// skipped.
func pemCertBlocks(data []byte) [][]byte {
	var blocks [][]byte
	rest := data
	for len(rest) > 0 {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		blocks = append(blocks, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: blk.Bytes}))
	}
	return blocks
}

// certExpired reports whether the single CERTIFICATE PEM block enc has
// expired as of now. An unparseable block is treated as NOT expired —
// dropping a CA we can't read would be more dangerous than keeping it.
func certExpired(enc []byte, now time.Time) bool {
	blk, _ := pem.Decode(enc)
	if blk == nil {
		return false
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return false
	}
	return !cert.NotAfter.After(now)
}

// CombineCABundles unions newCA into oldCA for the bootstrap path: it
// keeps every still-valid CA already in the bundle (so other replicas
// stay trusted across a rollout / chart re-install), prunes expired
// blocks, and ensures newCA is present. Output is canonical newline-
// separated PEM. main.go's self-signed bootstrap uses this so the very
// first patch has the same peer-preserving, expiry-pruning shape the
// Renewer's runtime rotation uses.
func CombineCABundles(oldCA, newCA []byte) []byte {
	return mergeCABundle(oldCA, newCA, nil, time.Now())
}
