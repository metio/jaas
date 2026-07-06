# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD

# The single source of the development toolchain: CI and local shells run every
# gate through this flake's devShell, so both use the exact tool versions
# pinned in flake.lock. Renovate keeps the lock fresh. The Go correctness gates
# (vet, staticcheck, gosec, gofumpt, govulncheck, arch-go, modernize, race
# tests) run in CI via the shared metio/ci reusable golang.yml; the same tools
# live here so a local run reproduces them, and the flake provides the toolchain
# for the build / docs / dashboards / fuzz / kind-smoke jobs that setup-go used
# to serve. envtest assets are assembled from nixpkgs, so controller tests run
# offline with no setup-envtest download.
{
  description = "jaas development environment";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-compat.url = "github:edolstra/flake-compat";
  };

  outputs =
    { self, nixpkgs, ... }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

      # arch-go (architecture rules, arch-go.yml) is not in nixpkgs.
      arch-go =
        pkgs:
        pkgs.buildGoModule rec {
          pname = "arch-go";
          version = "2.1.2";
          src = pkgs.fetchFromGitHub {
            owner = "arch-go";
            repo = "arch-go";
            rev = "v${version}";
            hash = "sha256-clwVZ/5PwUiD1LzRG6jGghQWcWZP3Pj3CzrdZiHUrIQ=";
          };
          vendorHash = "sha256-xIf+Ty1Pqa3oqqFLFsOv8Jz2bLOaIF+kjfGao05FhrM=";
        };

      # modernize (newer-Go idiom check) is a subpackage of x/tools' gopls
      # module; not in nixpkgs.
      modernize =
        pkgs:
        pkgs.buildGoModule rec {
          pname = "modernize";
          version = "0.47.0";
          src = pkgs.fetchFromGitHub {
            owner = "golang";
            repo = "tools";
            rev = "v${version}";
            hash = "sha256-JfrmKeIAhHhxMqOfh27w+T9PaBAIzh47wOokXmr1Z5Q=";
          };
          modRoot = "gopls";
          subPackages = [ "internal/analysis/modernize/cmd/modernize" ];
          vendorHash = "sha256-GF9KSCr2aMjczVKz9H2t5Gc2kF0wqmKenO7qa8TQw4o=";
        };

      # dadav/helm-schema (docs values reference, hack/gen-docs-data.sh) is not
      # in nixpkgs; same build as the helm-charts repo. Tags carry no `v`.
      helm-schema =
        pkgs:
        pkgs.buildGoModule rec {
          pname = "helm-schema";
          version = "0.23.4";
          src = pkgs.fetchFromGitHub {
            owner = "dadav";
            repo = "helm-schema";
            rev = version;
            hash = "sha256-btkkNzye9if4lF/YdhalbwA2/dcZArU6/9Hr0bTJf1M=";
          };
          vendorHash = "sha256-jbK+XD5CbjMQJUJCcKbNN8LhYuhuy+Z3XcCmgiYw25Y=";
        };
    in
    {
      packages = forAllSystems (pkgs: {
        arch-go = arch-go pkgs;
        modernize = modernize pkgs;
        helm-schema = helm-schema pkgs;
      });

      devShells = forAllSystems (
        pkgs:
        let
          # controller-runtime envtest wants a dir holding etcd, kube-apiserver,
          # and kubectl. Assemble it from nixpkgs so the assets are hermetic and
          # offline instead of downloaded by setup-envtest.
          kubebuilder-assets = pkgs.runCommand "kubebuilder-assets" { } ''
            mkdir -p $out
            ln -s ${pkgs.etcd}/bin/etcd $out/etcd
            ln -s ${pkgs.kubernetes}/bin/kube-apiserver $out/kube-apiserver
            ln -s ${pkgs.kubectl}/bin/kubectl $out/kubectl
          '';

          # The lint gate every metio repo shares byte-for-byte — lift into a
          # shared flake when the next repo needs the identical set.
          lintTools = with pkgs; [
            reuse
            typos
            yamllint
            actionlint
            shellcheck # actionlint shells out to it for run: blocks
            markdownlint-cli2
          ];

          # The Go toolchain + the correctness tools golang.yml runs in CI, so a
          # local `nix develop --command <tool>` reproduces the gate.
          goTools = [
            pkgs.go
            pkgs.go-tools # staticcheck
            (arch-go pkgs)
            (modernize pkgs)
          ]
          ++ (with pkgs; [
            gofumpt
            gosec
            govulncheck
          ]);

          # The dashboards + docs pipelines: go-jsonnet renders the grafonnet
          # dashboards; hugo builds the site; htmltest + biome + vale lint it;
          # helm-schema builds the chart values reference.
          docsTools = [
            (helm-schema pkgs)
          ]
          ++ (with pkgs; [
            go-jsonnet
            jsonnet-bundler # jb: vendors grafonnet for the dashboards render
            hugo
            htmltest
            biome
            vale
          ]);
        in
        {
          default = pkgs.mkShell {
            KUBEBUILDER_ASSETS = "${kubebuilder-assets}";
            packages = goTools ++ docsTools ++ lintTools ++ (with pkgs; [ jq ]);
            shellHook = ''
              echo "jaas devshell — go + static suite (staticcheck, gofumpt, gosec,"
              echo "  govulncheck, arch-go, modernize), envtest assets wired, plus the"
              echo "  docs/dashboards tools (jsonnet, hugo, htmltest, biome, vale,"
              echo "  helm-schema) and the lint gate. Run gates via nix develop --command."
            '';
          };
        }
      );

      formatter = forAllSystems (pkgs: pkgs.nixfmt-rfc-style);
    };
}
