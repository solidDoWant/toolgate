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

        # The gateway image for one Linux architecture: the static toolgate
        # binary that `make build-images` built (and checked with
        # govulncheck), CA certificates, and nothing else. Build it with
        # `--impure`: the binary is read from TOOLGATE_BINARY, the tag from
        # TOOLGATE_VERSION. The same binary and nixpkgs give the same image.
        mkImage = architecture:
          let
            binaryPath = builtins.getEnv "TOOLGATE_BINARY";
            imageVersion = builtins.getEnv "TOOLGATE_VERSION";
            binary =
              if binaryPath == "" then
                throw "toolgate: set TOOLGATE_BINARY to the linux/${architecture} toolgate binary and build with --impure (use `make build-images`)"
              else
                builtins.path { path = binaryPath; name = "toolgate-linux-${architecture}"; };
            root = pkgs.runCommand "toolgate-root-${architecture}" { } ''
              install -D -m 0555 ${binary} $out/bin/toolgate
              install -D -m 0444 ${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt $out/etc/ssl/certs/ca-certificates.crt
            '';
          in
          pkgs.dockerTools.buildLayeredImage {
            name = "ghcr.io/soliddowant/toolgate";
            tag = if imageVersion == "" then "0.0.0-dev" else imageVersion;
            inherit architecture;
            # An uncompressed docker archive: `docker load` and `crane push`
            # both read it.
            compressor = "none";
            # Only the root: the binary and the CA bundle are copied into it,
            # so no other store path (cacert's setup hook) ends up in the
            # image. Go reads /etc/ssl/certs/ca-certificates.crt by default.
            contents = [ root ];
            config = {
              Entrypoint = [ "/bin/toolgate" ];
              # A fixed non-root UID and GID; the image has no /etc/passwd.
              User = "65532:65532";
            };
          };

        # The control plane binaries for envtest
        # (sigs.k8s.io/controller-runtime/pkg/envtest): the dev shell points
        # KUBEBUILDER_ASSETS here, so tests never download binaries.
        # Kubernetes matches the k8s.io/client-go minor version in go.mod;
        # etcd 3.6 is the series the Kubernetes release is tested with.
        envtestAssets = pkgs.linkFarm "toolgate-envtest-assets" [
          { name = "kube-apiserver"; path = "${pkgs.kubernetes}/bin/kube-apiserver"; }
          { name = "etcd"; path = "${pkgs.etcd_3_6}/bin/etcd"; }
          { name = "kubectl"; path = "${pkgs.kubectl}/bin/kubectl"; }
        ];
      in
      {
        packages = pkgs.lib.optionalAttrs pkgs.stdenv.hostPlatform.isLinux {
          image-amd64 = mkImage "amd64";
          image-arm64 = mkImage "arm64";
        };

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
            # Flux, installed into the e2e cluster (scripts/e2e/run.sh).
            pkgs.fluxcd
            pkgs.kubernetes-helm
            pkgs.helm-docs
            pkgs.crane

            # Link and anchor check for the Markdown docs (make docs-check).
            pkgs.lychee

            # The web UI's stylesheet (make ui-css): Tailwind CSS v4's
            # standalone CLI, so the build needs no Node.
            pkgs.tailwindcss_4
          ] ++ pkgs.lib.optionals pkgs.stdenv.hostPlatform.isLinux [
            # Headless browser for the UI's browser tests (chromedp). nixpkgs
            # builds Chromium for Linux only; elsewhere the tests skip.
            pkgs.chromium
          ];

          KUBEBUILDER_ASSETS = "${envtestAssets}";
        });
      });
}
