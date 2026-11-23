{ ... }:
{
  perSystem =
    { pkgs, self', ... }:
    {
      devShells.default = pkgs.mkShell {
        inputsFrom = [ self'.packages.icebath-agent ];
        packages = with pkgs; [
          go
          gopls
          goreleaser
        ];
        shellHook = ''
          echo "Icebath dev shell. Build with: make build"
        '';
      };
    };
}
