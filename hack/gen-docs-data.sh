#!/usr/bin/env bash
# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD
#
# Generates every docs/data/* file the website's data-driven pages render:
#   - docs/data/flags.json       — the binary's CLI flags (via hack/flaggen)
#   - docs/data/helm-values.json — the jaas chart's flattened values schema
#   - docs/data/joi-values.json  — the joi chart's flattened values schema
#   - docs/data/joi-images.json  — the JOI image catalog (from jsonnet-oci-images)
#
# Run this through the flake's development shell before building the site:
#   nix develop --command hack/gen-docs-data.sh
#   nix develop --command hugo --minify --source docs
#
# The schemas are generated on-the-fly from each chart's Chart.yaml + values.yaml
# fetched from helm-charts' main branch (helm-schema, the same tool the chart
# repo runs at package time — the schema is never committed there). The docs
# workflow's daily schedule re-runs this, so a chart change reaches the site with
# no cross-repo trigger.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

data_dir="docs/data"
mkdir -p "${data_dir}"

charts_base="https://raw.githubusercontent.com/metio/helm-charts/main/charts"

echo "==> flaggen → ${data_dir}/flags.json"
go run ./hack/flaggen -o "${data_dir}/flags.json"

# Fetch a chart's Chart.yaml + values.yaml into a tempdir, generate its
# values.schema.json with helm-schema, then flatten it for the docs data file.
flatten_schema() {
  local chart="$1" out="$2"
  echo "==> ${chart} values.schema.json → ${out}"
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "${tmp}"' RETURN
  for f in Chart.yaml values.yaml; do
    curl --fail --silent --show-error --location \
      "${charts_base}/${chart}/${f}" -o "${tmp}/${f}"
  done
  helm-schema -c "${tmp}" -k additionalProperties
  jq -f hack/flatten-schema.jq "${tmp}/values.schema.json" > "${out}"
}

flatten_schema jaas "${data_dir}/helm-values.json"
flatten_schema joi  "${data_dir}/joi-values.json"

# The JOI image catalog comes from the jsonnet-oci-images manifest (the single
# source of truth its build pipeline maintains), so the page always lists the
# currently published images. The docs workflow's daily schedule re-fetches it.
echo "==> jsonnet-oci-images libraries.json → ${data_dir}/joi-images.json"
curl --fail --silent --show-error --location \
  "https://raw.githubusercontent.com/metio/jsonnet-oci-images/main/libraries.json" \
  | jq -f hack/joi-images.jq > "${data_dir}/joi-images.json"

echo "==> docs data generated"
