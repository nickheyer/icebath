# The icebath binary with the dashboard bundle embedded by webdist and Matrix encryption compiled in by goolm
{
  lib,
  callPackage,
  buildGoModule,
  makeWrapper,
  web,
  rev ? null,
  extraPythonPackages ? [ ],
}:
let
  version = if rev != null then "0-unstable-${builtins.substring 0 12 rev}" else "0-unstable";
  python = (callPackage ./pythonLock.nix { }).interpreter;
  # Site directories of the plugin packages and every Python module they require, for the plugin host
  pythonPath = lib.makeSearchPath python.sitePackages (python.pkgs.requiredPythonModules extraPythonPackages);
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
  vendorHash = "sha256-lOM8xlhVJTgqRJWAieHLac11iwou4H1cas5aNm86+8M=";
  subPackages = [ "cmd/icebath" ];
  tags = [
    "webdist"
    "goolm"
  ];
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
  nativeBuildInputs = lib.optional (extraPythonPackages != [ ]) makeWrapper;
  postInstall = ''
    printf 'nix\n' > $out/bin/.install_method
  ''
  + lib.optionalString (extraPythonPackages != [ ]) ''
    wrapProgram $out/bin/icebath --suffix PYTHONPATH : "${pythonPath}"
  '';
  doCheck = false;
  passthru = {
    inherit web python extraPythonPackages;
  };
  meta = {
    description = "Tiling agentic harness and netrunner toolkit";
    homepage = "https://icebath.app";
    license = lib.licenses.mit;
    mainProgram = "icebath";
  };
}
