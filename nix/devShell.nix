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
          nodejs_latest
          goreleaser
        ];
        shellHook = ''
          echo "Icebath dev shell. Build with: go run ./cmd/icebath-build web && go build -tags webdist ./cmd/icebath"
        '';
      };
    };
}
