# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD

# Regenerates every controller-gen artifact this repo commits:
#   - api/v1/zz_generated.deepcopy.go — DeepCopy implementations
#   - config/crd/bases/*.yaml         — the CustomResourceDefinitions
#
# Run it after any change to api/v1 and commit the result. The `generated` job
# in verify.yml runs this same command and fails on a diff, so the gate cannot
# disagree with the recipe.
#
# controller-gen comes from the go.mod `tool` directive, so its version is
# pinned there and Renovate's gomod manager bumps it. The release Dockerfile
# runs the same CRD command to bake the CRDs into the image under /crds.
#
# No headerFile is passed for the deepcopy: REUSE.toml carries the SPDX
# annotation for api/**/zz_generated.deepcopy.go precisely because
# controller-gen rewrites the file wholesale and would clobber an inline header.

go tool controller-gen object paths=./api/v1/...
go tool controller-gen crd paths=./api/... output:crd:dir=./config/crd/bases
