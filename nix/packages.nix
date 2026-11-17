{ inputs, ... }:
{
  perSystem =
    { pkgs, ... }:
    let
      rev = inputs.self.rev or inputs.self.dirtyRev or null;
      icebath-agent = pkgs.callPackage ./icebath-agent.nix { inherit rev; };
    in
    {
      packages = {
        default = icebath-agent;
        inherit icebath-agent;
      };
    };
}
