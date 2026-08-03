{
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = {
    self,
    nixpkgs,
    flake-utils,
  }:
    flake-utils.lib.eachDefaultSystem (
      system: let
        pkgs = import nixpkgs {inherit system;};
      in {
        checks.default = pkgs.buildGoModule {
          pname = "gocan-check";
          version = "0.0.0";
          src = self;
          vendorHash = "sha256-FhZA7f1iwO7kroBllIEzvdd/3f7kIOn9P2HxG0TmyNA=";

          env.CGO_ENABLED = 1;
          excludedPackages =
            ["internal/devicechange"]
            ++ pkgs.lib.optionals (!pkgs.stdenv.hostPlatform.isLinux) ["drivers/socketcan"];
          checkFlags = ["-race"];
          postCheck = "go vet ./...";
        };

        devShells.default = pkgs.mkShell {
          packages = [
            pkgs.go
            pkgs.gopls
          ];
        };
      }
    );
}
