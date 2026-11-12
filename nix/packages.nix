{ inputs, ... }:
{
  perSystem =
    { pkgs, ... }:
    let
      rev = inputs.self.rev or inputs.self.dirtyRev or null;
      web = pkgs.callPackage ./web.nix { };
      icebath-agent = pkgs.callPackage ./icebath-agent.nix { inherit web rev; };
    in
    {
      packages = {
        default = icebath-agent;
        inherit icebath-agent web;
      };
    };
}
