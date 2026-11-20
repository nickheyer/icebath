# The icebath binary with Matrix encryption compiled in by goolm
{
  lib,
  callPackage,
  buildGoModule,
  makeWrapper,
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
      ../cli-config.yaml.example
      ../.env.example
      ../LICENSE
      ../THIRD_PARTY_NOTICES
    ];
  };
  vendorHash = "sha256-8YhWpB4posMj45EeDvhHDksvsvst2jv/w7CKyers2GQ=";
  subPackages = [ "cmd/icebath" ];
  tags = [
    "goolm"
  ];
  env.CGO_ENABLED = 0;
  ldflags = [
    "-s"
    "-w"
    "-X main.version=${version}"
  ];
  nativeBuildInputs = lib.optional (extraPythonPackages != [ ]) makeWrapper;
  postInstall = ''
    printf 'nix\n' > $out/bin/.install_method
    install -Dm644 cli-config.yaml.example $out/bin/cli-config.yaml.example
    install -Dm644 .env.example $out/bin/.env.example
    install -Dm644 LICENSE $out/share/licenses/icebath/LICENSE
    install -Dm644 THIRD_PARTY_NOTICES $out/share/licenses/icebath/THIRD_PARTY_NOTICES
  ''
  + lib.optionalString (extraPythonPackages != [ ]) ''
    wrapProgram $out/bin/icebath --suffix PYTHONPATH : "${pythonPath}"
  '';
  doCheck = false;
  passthru = {
    inherit python extraPythonPackages;
  };
  meta = {
    description = "Tiling agentic harness and netrunner toolkit";
    homepage = "https://icebath.app";
    license = lib.licenses.mit;
    mainProgram = "icebath";
  };
}
