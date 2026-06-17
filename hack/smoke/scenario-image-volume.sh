#!/usr/bin/env bash
# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD
#
# OCI image-volume runtime check: this is the ONLY smoke scenario that exercises
# the chart's `volumes[].image` library mounts (every other job DISABLES them
# because the ImageVolume feature gate is off by default on kind). It runs in the
# standalone HTTP-renderer mode (operator.enabled=false) with a real
# additionalLibraries OCI mount; reaching Ready proves the kubelet pulled and
# mounted the OCI image as a volume — without ImageVolume support the pod wedges
# in ContainerCreating forever. The workflow gates this job to k8s >= 1.33
# (containerd 2.x + ImageVolume beta-on); below that it skips. Env: JNS (jaas
# install namespace), MOUNT (the additionalLibraries key / mount subdir).
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"; . "$DIR/lib.sh"
JNS="${JNS:-jaas-system}"; MOUNT="${MOUNT:-xtd}"

log "wait for the renderer deployment to roll out (proves the OCI image-volume mounted)"
# `helm install --wait` already gates on this, but assert explicitly so a failure
# points straight here rather than at the install step.
kubectl -n "$JNS" rollout status deploy/jaas --timeout=180s

POD="$(kubectl -n "$JNS" get pods -l app.kubernetes.io/name=jaas \
  -o jsonpath='{.items[0].metadata.name}')"
[ -n "$POD" ] || die "no jaas renderer pod found"
log "renderer pod: $POD"

# A pod stuck on a failed/absent image-volume mount never reaches Running with
# all containers Ready — the kubelet holds it in ContainerCreating. Assert the
# pod is Running so the failure mode (no ImageVolume support) is named here.
PHASE="$(kubectl -n "$JNS" get pod "$POD" -o jsonpath='{.status.phase}')"
[ "$PHASE" = "Running" ] || { kubectl -n "$JNS" describe pod "$POD"; die "renderer pod is $PHASE, not Running (OCI image-volume likely did not mount)"; }

# The chart mounts additionalLibraries beneath paths.libraries (default
# /srv/libraries) under the map key as a subdirectory at .../$MOUNT. The runtime
# base is distroless (gcr.io/distroless/static:nonroot) — no shell, no `ls` — so
# a listing via `kubectl exec` is best-effort only; the Running/Ready gate above
# is what proves the kubelet mounted the volume.
kubectl -n "$JNS" exec "$POD" -- ls -la "/srv/libraries/${MOUNT}/" 2>/dev/null \
  || log "(skipping in-container ls — distroless image has no shell; Running/Ready already proves the mount)"

log "image-volume mount verified"
