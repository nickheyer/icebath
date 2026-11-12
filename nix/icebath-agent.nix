# The icebath binary with the dashboard bundle embedded by the webdist build tag
{
  lib,
  buildGoModule,
  web,
  rev ? null,
}:
let
  version = if rev != null then "0-unstable-${builtins.substring 0 12 rev}" else "0-unstable";
in
buildGoModule {
  pname = "icebath-agent";
  inherit version;
  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      ../go.sum
      ../cmd
      ../pkg
      ../third_party
    ];
  };
  vendorHash = "sha256-VFIkMT8wdu0vYlB000FE4lSlH8IXTuYs4hq8ELkR3ow=";
  subPackages = [ "cmd/icebath" ];
  tags = [ "webdist" ];
  env.CGO_ENABLED = 0;
  ldflags = [
    "-s"
    "-w"
    "-X main.version=${version}"
  ];
  preBuild = ''
    rm -rf cmd/icebath/web_dist
    cp -r ${web} cmd/icebath/web_dist
    chmod -R u+w cmd/icebath/web_dist
  '';
  postInstall = ''
    printf 'nix\n' > $out/bin/.install_method
  '';
  doCheck = false;
  passthru = { inherit web; };
  meta = {
    description = "Tiling agentic harness and netrunner toolkit";
    homepage = "https://icebath.app";
    license = lib.licenses.mit;
    mainProgram = "icebath";
  };
}
