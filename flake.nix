{
  description = "toolgate development environment";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};

        # Evaluated as a path flake (`path:...`, or builtins.getFlake on a
        # bare path), Nix copies the whole directory into the store,
        # gitignored build output and caches included, on every evaluation of
        # a changed tree. Git sources carry rev (clean) or dirtyRev (dirty);
        # path sources carry neither, so the dev shell refuses them.
        requireGitSource = value:
          if self ? rev || self ? dirtyRev then
            value
          else
            throw ''
              toolgate: this flake was evaluated as a path flake, so Nix copied the whole directory to ${self.outPath}.
              Run `nix develop` inside the checkout or worktree instead, or use builtins.getFlake "git+file://''${toString ./.}".
              Reclaim the space with: nix store delete ${self.outPath}
            '';

        # The Go toolchain is nixpkgs' Go. go.mod's `toolchain` directive (or
        # its `go` line when there is none) must not ask for a newer one: CI
        # and the agents build with this Go, never with a downloaded
        # toolchain.
        goMod = builtins.readFile ./go.mod;
        goModToolchain = builtins.match ".*\ntoolchain go([0-9.]+)\n.*" goMod;
        goModGo = builtins.match ".*\ngo ([0-9.]+)\n.*" goMod;
        minimumGoVersion =
          if goModToolchain != null then builtins.head goModToolchain
          else if goModGo != null then builtins.head goModGo
          else throw "toolgate: go.mod has neither a `toolchain goX.Y.Z` nor a `go X.Y.Z` line";
        go =
          if pkgs.lib.versionAtLeast pkgs.go.version minimumGoVersion then pkgs.go
          else throw "toolgate: go.mod asks for Go ${minimumGoVersion}, but nixpkgs has Go ${pkgs.go.version}; update flake.lock (nix flake update nixpkgs) or lower go.mod.";
      in
      {
        devShells.default = requireGitSource (pkgs.mkShell {
          packages = [
            go
            # golangci-lint refuses to load code whose go.mod asks for a newer
            # Go than golangci-lint was built with. nixpkgs builds it with the
            # newest Go it supports, so it stays ahead of `go`; if go.mod ever
            # overtakes it, build it with `go` instead (override its
            # buildGo1NNModule argument).
            pkgs.golangci-lint
            pkgs.govulncheck
            pkgs.gnumake
            # git pinned by flake.lock, so local runs and CI use one version.
            pkgs.git
            # gh is deliberately NOT provided here: the coder VM ships a gh
            # wrapper in ~/.local/bin that fetches a fresh GitHub App token
            # per call, and a dev-shell gh would shadow it on PATH.
            pkgs.jq
            pkgs.yq-go
            # Agents and the scheduler need python3; the VM image has none.
            pkgs.python3
            pkgs.shellcheck

            # Code generation (make generate): CRDs and deepcopy, SQL queries,
            # migrations.
            pkgs.kubernetes-controller-tools
            pkgs.sqlc
            pkgs.goose

            # Kubernetes: kind clusters for e2e, the chart, images.
            pkgs.kind
            pkgs.kubectl
            pkgs.kubernetes-helm
            pkgs.helm-docs
            pkgs.crane

            # Link and anchor check for the Markdown docs (make docs-check).
            pkgs.lychee
          ];
        });
      });
}
