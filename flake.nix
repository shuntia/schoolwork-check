{
  description = "schoolwork-check: Canvas, Google Classroom and course calendars mirrored into note";
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  outputs = { self, nixpkgs }:
    let
      system = "x86_64-linux";
      pkgs = nixpkgs.legacyPackages.${system};
      pkg = pkgs.buildGo126Module {
        pname = "schoolwork-check";
        version = "0-unstable-${self.lastModifiedDate or "dirty"}";
        src = pkgs.lib.fileset.toSource {
          root = ./.;
          fileset = pkgs.lib.fileset.unions [ ./go.mod ./go.sum ./cmd ./internal ];
        };
        # Bump when go.mod's requirements change: the build prints the new hash.
        vendorHash = "sha256-452XQ2kkGQMWhYmJF28EkJezLBboY9qfQH95jlVJpYc=";
        subPackages = [ "cmd/schoolwork-check" ];
        ldflags = [ "-s" "-w" ];
        meta = {
          description = "Aggregate Canvas and Google Classroom tasks into note";
          mainProgram = "schoolwork-check";
          platforms = pkgs.lib.platforms.linux;
        };
      };
    in {
      packages.${system} = { default = pkg; schoolwork-check = pkg; };
      nixosModules.default = import ./nix/module.nix self;
      devShells.${system}.default = pkgs.mkShell { packages = [ pkgs.go ]; };
    };
}
