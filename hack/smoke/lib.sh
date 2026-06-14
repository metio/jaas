# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD
#
# shellcheck shell=bash
#
# Shared helpers for the operator end-to-end smoke scenarios. Sourced by the
# scenario-*.sh scripts. These encode operator BEHAVIOUR (apply a CR, wait for
# a status, verify the published ExternalArtifact) and are deliberately agnostic
# to HOW jaas was deployed — the calling workflow owns that (which chart, which
# image). The jaas repo runs them against the dev binary + released chart; the
# helm-charts repo runs the same scripts (checked out from jaas at the released
# tag) against the dev chart + released binary. Assumes kubectl is already
# pointed at the target cluster and jaas is deployed.

set -euo pipefail

log() { printf '\n=== %s ===\n' "$*" >&2; }
die() { printf 'SMOKE FAIL: %s\n' "$*" >&2; exit 1; }

# grant_tenant_publish_rbac <ns> [sa] — grant the tenant ServiceAccount (default
# "default") the RBAC the operator needs while impersonating it to publish: get
# / create / update the snippet's ExternalArtifact and write its status, plus
# delete for the finalizer's Withdraw. The operator acts AS the tenant SA (no
# `impersonate` verb on its own SA), so without this every reconcile fails
# RBACDenied at the publish step and the snippet never goes Ready.
grant_tenant_publish_rbac() {
  local ns=$1 sa=${2:-default}
  kubectl apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: { namespace: ${ns}, name: jaas-tenant-publish }
rules:
  - apiGroups: ["source.toolkit.fluxcd.io"]
    resources: ["externalartifacts", "externalartifacts/status"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: { namespace: ${ns}, name: jaas-tenant-publish }
subjects:
  - { kind: ServiceAccount, name: ${sa}, namespace: ${ns} }
roleRef: { apiGroup: rbac.authorization.k8s.io, kind: Role, name: jaas-tenant-publish }
EOF
}

# ready_status <kind> <name> <ns> — echoes the Ready condition's status (or "").
ready_status() {
  kubectl -n "$3" get "$1" "$2" \
    -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true
}

# ready_reason <kind> <name> <ns> — echoes the Ready condition's reason (or "").
ready_reason() {
  kubectl -n "$3" get "$1" "$2" \
    -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}' 2>/dev/null || true
}

# wait_ready <kind> <name> <ns> [polls] [sleep] — block until Ready=True.
wait_ready() {
  local kind=$1 name=$2 ns=$3 polls=${4:-60} s=${5:-5} i
  for i in $(seq 1 "$polls"); do
    [ "$(ready_status "$kind" "$name" "$ns")" = "True" ] && { log "$kind/$name Ready=True after $i polls"; return 0; }
    sleep "$s"
  done
  kubectl -n "$ns" describe "$kind" "$name" >&2 || true
  die "$kind/$name did not reach Ready=True"
}

# wait_reason <kind> <name> <ns> <reason> [polls] [sleep] — block until the
# Ready condition's reason equals <reason>.
wait_reason() {
  local kind=$1 name=$2 ns=$3 want=$4 polls=${5:-60} s=${6:-2} i
  for i in $(seq 1 "$polls"); do
    [ "$(ready_reason "$kind" "$name" "$ns")" = "$want" ] && { log "$kind/$name Ready reason=$want after $i polls"; return 0; }
    sleep "$s"
  done
  kubectl -n "$ns" describe "$kind" "$name" >&2 || true
  die "$kind/$name Ready reason never became $want"
}

# apply_retry [polls] [sleep] — kubectl apply from stdin, retrying while the
# admission webhook is still coming up. The operator patches its VWC caBundle at
# startup (so the caBundle appears early), but its webhook server only starts
# listening once the manager is running and has won leader election — an apply in
# that window fails with "failed calling webhook … connection refused". Those are
# retried; any other error fails immediately so real rejections aren't masked.
apply_retry() {
  local polls=${1:-30} s=${2:-2} i out manifest
  manifest="$(cat)"
  for i in $(seq 1 "$polls"); do
    if out="$(printf '%s' "$manifest" | kubectl apply -f - 2>&1)"; then
      printf '%s\n' "$out" >&2
      return 0
    fi
    if printf '%s' "$out" | grep -qiE 'failed calling webhook|connection refused|no endpoints available|EOF|i/o timeout'; then
      sleep "$s"; continue
    fi
    printf '%s\n' "$out" >&2
    return 1
  done
  printf '%s\n' "$out" >&2
  die "kubectl apply never succeeded (webhook admission did not come up in time)"
}

# ea_url <name> <ns> — echoes the snippet's ExternalArtifact URL (or "").
ea_url() {
  kubectl -n "$2" get externalartifact "$1" -o jsonpath='{.status.artifact.url}' 2>/dev/null || true
}

# fetch_artifact <url> [grep_pattern] — fetch the artifact tarball from a
# throwaway in-cluster curl pod. With grep_pattern set, also untars and asserts
# the pattern appears in the rendered output (the Publisher writes rendered.json
# into the tarball). curl image kept pinned and long-form per repo convention.
fetch_artifact() {
  local url=$1 pat=${2:-} script
  if [ -n "$pat" ]; then
    script="set -eu; curl -fsSL '$url' -o /tmp/a.tgz; tar -xzf /tmp/a.tgz -C /tmp; grep -q '$pat' /tmp/rendered.json; echo 'artifact content verified'"
  else
    script="set -eu; curl -fsSL '$url' -o /tmp/a.tgz; echo 'artifact fetched OK'"
  fi
  kubectl run --rm -i --restart=Never \
    --image=docker.io/curlimages/curl:8.10.1 "smoke-curl-$$" -- sh -c "$script"
}

# has_event <name> <ns> <type> <reason> — true if a matching event exists.
has_event() {
  kubectl -n "$2" get events --field-selector "involvedObject.name=$1" \
    -o jsonpath="{.items[?(@.type==\"$3\")].reason}" 2>/dev/null | grep -qw "$4"
}
