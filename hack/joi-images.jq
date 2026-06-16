# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD
#
# Transforms the jsonnet-oci-images manifest (libraries.json) into the catalog
# rows the JOI images page renders: {name, image, source, upstream, description},
# sorted by name. The image reference follows the jsonnet-oci-images naming
# convention `ghcr.io/metio/joi-<org>-<repo>`.
#
# Usage: jq -f hack/joi-images.jq libraries.json > docs/data/joi-images.json

[ .[] | {
    name: .name,
    image: ("ghcr.io/metio/joi-" + .org + "-" + .repo),
    source: .source,
    upstream: (.org + "/" + .repo),
    description: (.description // "")
} ] | sort_by(.name)
