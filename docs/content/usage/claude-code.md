---
title: Claude Code skill
description: Install the JaaS Claude Code skill so Claude authors and operates JsonnetSnippet and JsonnetLibrary resources with accurate, current field knowledge.
tags: [contributing, claude, skill, plugin]
---

Install the JaaS skill into [Claude Code](https://claude.com/claude-code) and
Claude gains working knowledge of JaaS: it authors and edits `JsonnetSnippet` and
`JsonnetLibrary` resources, wires snippets to inline files or a Flux source
(`GitRepository` / `OCIRepository` / `Bucket` / `ExternalArtifact`), chains one
snippet's output into another, pairs JaaS with the grafana-operator or
stageset-controller, configures per-snippet ServiceAccount impersonation, and
calls the HTTP rendering endpoint with top-level arguments and external variables.
The skill points Claude at the live documentation and a compact cheat-sheet, so it
reaches for current fields and defaults instead of guessing.

The skill ships in this repository under `skills/jaas/` (`SKILL.md` plus
`references/reference.md`), packaged as a Claude Code plugin via the manifests in
`.claude-plugin/`.

## Install

Add this repository as a plugin marketplace, then install the plugin:

```text
/plugin marketplace add metio/jaas
/plugin install jaas@jaas
```

The first command registers the `jaas` marketplace from
[github.com/metio/jaas](https://github.com/metio/jaas); the second installs the
`jaas` plugin from it. Claude activates the skill automatically whenever a
repository holds `JsonnetSnippet` or `JsonnetLibrary` manifests or JaaS is
otherwise in play.

## What it grants Claude

The skill's only tools are `kubectl` and `curl`, so Claude can inspect snippets
and call the renderer but takes no broader action. It treats
[the documentation site](https://jaas.projects.metio.wtf/) as the source of truth
— including the machine-readable `/llms.txt` index and the concatenated
`/llms-full.txt` — and falls back to the bundled cheat-sheet for quick field
lookups. The guidance covers the authoring gotchas that are easy to get wrong:
exactly one of `spec.files` or `spec.sourceRef`, the library alias field being
`importPath` (not `as`), single-layer `OCIRepository` artifacts, and the
ServiceAccount impersonation RBAC each snippet needs.
