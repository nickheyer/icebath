{ inputs, ... }:
{
  flake.overlays.default = final: _: {
    icebath-agent = inputs.self.packages.${final.stdenv.hostPlatform.system}.default;
  };
}
