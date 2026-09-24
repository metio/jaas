/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/metio/jaas/internal/opstate"
	"k8s.io/client-go/rest"
)

const (
	// superviseBaseDelay is the wait after the first failed manager start.
	superviseBaseDelay = time.Second
	// superviseMaxDelay caps the exponential growth. An operator whose RBAC or
	// NetworkPolicy is fixed out of band recovers within this long at worst,
	// which is the number to weigh against the request rate a wedged pod puts
	// on an apiserver that may itself be the thing recovering.
	superviseMaxDelay = 5 * time.Minute
	// superviseJitterFraction spreads retries across replicas so a fleet that
	// lost the apiserver together does not reconnect in lockstep.
	superviseJitterFraction = 0.2
)

// Supervise runs the operator manager and restarts it for as long as ctx
// lives, reporting each transition through state.
//
// Everything the manager needs from the apiserver before it can serve —
// discovery for the field indexes, the RESTMapper lookups behind the Flux
// source watches — happens while it is being built, so an apiserver that is
// unreachable, or an RBAC grant that is missing, fails the build outright.
// Treating that as fatal would end the process, and with it the Jsonnet
// renderer and the artifact server, neither of which needs the apiserver at
// all; the kubelet would then restart the pod into the same failure for as long
// as the cause lasts. Retrying in place keeps the rest of the binary serving,
// and the manager comes up on its own once the cause is cleared.
//
// A lost leader-election lease arrives here the same way, as mgr.Start
// returning: the next attempt blocks on acquiring the lease again, which is the
// behaviour the lease exists for.
func Supervise(ctx context.Context, cfg Config, restCfg *rest.Config, state *opstate.State) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if state == nil {
		state = opstate.New()
	}
	SetOperatorState(state)
	// The manager signals a synced cache through OnReady, which is also what
	// flips the pod's readiness probe. Availability means the same thing, so it
	// hangs off the same signal rather than off "Run has been called".
	onReady := cfg.OnReady
	cfg.OnReady = func() {
		if onReady != nil {
			onReady()
		}
		if state.MarkAvailable() {
			logger.Info("Operator available", slog.Int("attempt", state.Snapshot().Attempts))
		}
	}
	supervise(ctx, state, logger, func(ctx context.Context) error {
		return Run(ctx, cfg, restCfg)
	}, superviseDelay)
}

// supervise is Supervise with the manager and the backoff replaced by
// parameters, so the retry behaviour can be exercised without an apiserver and
// without waiting out real delays.
func supervise(
	ctx context.Context,
	state *opstate.State,
	logger *slog.Logger,
	run func(context.Context) error,
	delayFor func(attempt int) time.Duration,
) {
	for attempt := 1; ; attempt++ {
		state.RecordAttempt()
		errCh := make(chan error, 1)
		go func() { errCh <- run(ctx) }()

		var err error
		select {
		case err = <-errCh:
		case <-ctx.Done():
			// A manager that reached a synced cache is draining its in-flight
			// reconciles, so it is awaited — that window is the point of a
			// graceful shutdown, and run() caps it. A manager still being built
			// has nothing to drain, and it can be stuck in an apiserver call
			// that ignores the context: client-go's discovery honours the REST
			// client's own timeout rather than ours, so against an unreachable
			// apiserver the build takes about ten seconds to fail. Waiting that
			// out would add it to every pod deletion in a degraded cluster, so
			// the orphan is left to finish on its own.
			if !state.Available() {
				return
			}
			err = <-errCh
		}
		if ctx.Err() != nil {
			// Shutdown, not failure: the context that stopped the manager is
			// the one the process is exiting on.
			return
		}
		recordOperatorStartFailure()
		reason := "operator manager stopped"
		if err != nil && !errors.Is(err, context.Canceled) {
			reason = err.Error()
		}
		// A manager that had been reconciling starts its backoff over: the
		// cause is fresh, and the delay that had grown during an earlier
		// outage says nothing about this one.
		if state.MarkUnavailable(reason) {
			attempt = 1
		}
		delay := delayFor(attempt)
		// The backoff is what keeps this from flooding: the retries thin out
		// to one every superviseMaxDelay, and each carries the reason, so a
		// wedged operator stays visible without a rate limiter of its own.
		if attempt == 1 {
			logger.Error("Operator unavailable, restarting",
				slog.String("reason", reason), slog.Duration("retryIn", delay))
		} else {
			logger.Warn("Operator still unavailable, restarting",
				slog.String("reason", reason), slog.Int("attempt", attempt), slog.Duration("retryIn", delay))
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// superviseDelay is the wait before the attempt-th restart: exponential from
// superviseBaseDelay, capped at superviseMaxDelay, spread by a jitter fraction.
func superviseDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	// Doubling in a loop that stops at the cap keeps the growth away from the
	// range where a shift would overflow, whatever the attempt count reaches.
	delay := superviseBaseDelay
	for i := 1; i < attempt && delay < superviseMaxDelay; i++ {
		delay *= 2
	}
	delay = min(delay, superviseMaxDelay)
	spread := float64(delay) * superviseJitterFraction
	// #nosec G404 -- spreading retries across replicas, not a security decision.
	return time.Duration(float64(delay) - spread + rand.Float64()*2*spread)
}
