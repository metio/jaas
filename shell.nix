# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD

# nix-shell compatibility: exposes the flake's devShell to plain `nix-shell`.
# The flake-compat pin is read from flake.lock, so Renovate's lock updates
# cover it and nothing here needs manual bumping.
(import (
  let
    lock = builtins.fromJSON (builtins.readFile ./flake.lock);
    node = lock.nodes.flake-compat.locked;
  in
  fetchTarball {
    url = "https://github.com/edolstra/flake-compat/archive/${node.rev}.tar.gz";
    sha256 = node.narHash;
  }
) { src = ./.; }).shellNix
