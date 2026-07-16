# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD

# Live-reloading documentation server on http://localhost:1212.
#
# The resource and publish directories are redirected out of the repo on
# purpose. Hugo fingerprints CSS and stamps a subresource-integrity hash keyed to
# the baseURL, and caches the result in docs/resources/_gen. The server's baseURL
# (localhost) differs from the production build's, so sharing that cache lets one
# serve the other's hashed, prod-URL'd CSS — the browser then blocks it on an SRI
# mismatch and the page renders unstyled. Separate directories keep a `website`
# build and a `serve` session independent, in either order.

env HUGO_RESOURCEDIR=/tmp/hugo-resources-dev HUGO_PUBLISHDIR=/tmp/hugo-public-dev \
  hugo server --port 1212 --minify --printI18nWarnings --printPathWarnings --printUnusedTemplates --source docs
