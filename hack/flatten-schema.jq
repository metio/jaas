# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD
#
# Flattens a Helm chart values.schema.json into an ordered array of
# {path, type, default, description}, one row per leaf-or-object property.
#
# Recurses `.properties`, joining nested keys with "." so a value like
# `image.repository` renders as a single dotted path. Both the object node and
# its children are emitted, so an operator sees the grouping object's own
# description alongside its fields. `type` arrays (e.g. ["object","null"]) are
# joined with "|". Defaults are rendered as compact JSON; an absent default
# becomes the empty string, which the Hugo shortcode renders as "(empty)".
#
# Usage: jq -f hack/flatten-schema.jq values.schema.json > out.json

# walk(prefix; node) -> stream of rows for node and its descendants.
def walk(prefix; node):
  (node.properties // {}) as $props
  | $props
  | to_entries[]
  | (if prefix == "" then .key else prefix + "." + .key end) as $path
  | .value as $v
  | (
      {
        path: $path,
        type: (
          ($v.type // "") as $t
          | if ($t | type) == "array" then ($t | join("|")) else $t end
        ),
        default: (
          if ($v | has("default"))
          then ($v.default | if type == "string" then . else tojson end)
          else ""
          end
        ),
        description: (($v.description // "") | gsub("\n"; " "))
      }
    ),
    walk($path; $v)
  ;

[ walk(""; .) ]
