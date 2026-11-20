{
  description = "Icebath Agent - tiling agentic harness and netrunner toolkit";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-parts = {
      url = "github:hercules-ci/flake-parts";
      inputs.nixpkgs-lib.follows = "nixpkgs";
    };
  };

  outputs =
    inputs:
    inputs.flake-parts.lib.mkFlake { inherit inputs; } {
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
      ];
      imports = [
        ./nix/packages.nix
        ./nix/overlays.nix
        ./nix/nixosModules.nix
        ./nix/homeManagerModules.nix
        ./nix/devShell.nix
      ];
    };
}
