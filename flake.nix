# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD

# The single source of the development toolchain: CI and local shells run every
# gate through this flake's devShell, so both use the exact tool versions pinned
# in flake.lock. The shared lint gate, the three from-source Go tools (arch-go,
# modernize, helm-schema), and the org-wide nixpkgs pin come from the metio/ci
# flake; Renovate keeps the lock fresh. The Go correctness gates run in CI via
# the shared metio/ci golang.yml; the same tools live here so a local
# `nix develop --command <tool>` reproduces the gate, and the flake serves the
# build / docs / dashboards / fuzz / kind-smoke jobs too. KUBEBUILDER_ASSETS is
# assembled from nixpkgs, so controller tests run offline with no setup-envtest
# download.
{
  description = "jaas development environment";

  inputs = {
    devshell.url = "github:metio/nix-devshell";
    nixpkgs.follows = "devshell/nixpkgs";
    flake-compat.follows = "devshell/flake-compat";
  };

  outputs =
    {
      self,
      nixpkgs,
      devshell,
      ...
    }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      devShells = forAllSystems (
        pkgs:
        let
          # The Go toolchain + the correctness tools golang.yml runs in CI, so a
          # local `nix develop --command <tool>` reproduces the gate. arch-go and
          # modernize come from the shared metio/ci flake (built from source, not
          # in nixpkgs).
          goTools = [
            pkgs.go
            pkgs.go-tools # staticcheck
            (devshell.lib.arch-go pkgs)
            (devshell.lib.modernize pkgs)
          ]
          ++ (with pkgs; [
            gofumpt
            gosec
            govulncheck
          ]);

          # The dashboards + docs pipelines: go-jsonnet renders the grafonnet
          # dashboards; hugo builds the site; htmltest + biome + vale lint it;
          # helm-schema (from metio/ci) builds the chart values reference; cosign
          # keyless-signs the pushed dashboard image (dashboards.yml).
          docsTools = [
            (devshell.lib.helm-schema pkgs)
          ]
          ++ (with pkgs; [
            go-jsonnet
            jsonnet-bundler # jb: vendors grafonnet for the dashboards render
            hugo
            htmltest
            biome
            vale
            cosign
          ]);
        in
        {
          default = devshell.lib.mkDevShell {
            inherit pkgs;
            packages = goTools ++ docsTools ++ (with pkgs; [ jq ]);
            # controller-runtime envtest wants a dir holding etcd, kube-apiserver,
            # and kubectl; the shared flake assembles it from nixpkgs, so the
            # assets are hermetic and offline instead of downloaded.
            env.KUBEBUILDER_ASSETS = "${devshell.lib.kubebuilderAssets pkgs}";
            menu = ''
              echo "jaas — go + static suite (staticcheck, gofumpt, gosec, govulncheck,"
              echo "  arch-go, modernize), envtest assets wired, plus the docs/dashboards"
              echo "  tools (jsonnet, hugo, htmltest, biome, vale, helm-schema, cosign)."
            '';
          };
        }
      );

      formatter = forAllSystems (pkgs: pkgs.nixfmt-rfc-style);
    };
}
