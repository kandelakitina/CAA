{
  description = "Development environment for Go and htmx applications";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flakelight.url = "github:nix-community/flakelight";
  };

  outputs =
    { flakelight, ... }@inputs:
    flakelight ./. {
      inherit inputs;

      devShell = {
        packages =
          pkgs: with pkgs; [
            go
            gopls
            gofumpt
            golangci-lint
            air
            delve

            tailwindcss
            html-tidy
          ];

        shellHook = ''
          export GOPATH="$PWD/.go"
          export GOBIN="$GOPATH/bin"
          export PATH="$GOBIN:$PATH"

          echo "Go + htmx development shell ready"
        '';
      };
    };
}
