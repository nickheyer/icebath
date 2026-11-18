# The runtime only venv pm keeps beside the binary, the nemo-relay wheel pkg/pm/runtime_requirements.json pins installed into the lock's Python
{
  lib,
  stdenv,
  fetchurl,
  unzip,
  autoPatchelfHook,
  python,
}:
let
  requirements = builtins.fromJSON (builtins.readFile ../pkg/pm/runtime_requirements.json);
  relay = builtins.head (builtins.filter (r: r.name == "nemo-relay") requirements);
  wheelSuffix =
    {
      x86_64-linux = "-manylinux_2_17_x86_64.manylinux2014_x86_64.whl";
      aarch64-linux = "-manylinux_2_17_aarch64.manylinux2014_aarch64.whl";
      aarch64-darwin = "-macosx_11_0_arm64.whl";
    }
    .${stdenv.hostPlatform.system}
      or (throw "pkg/pm/runtime_requirements.json pins no nemo-relay wheel for ${stdenv.hostPlatform.system}");
  file = builtins.head (builtins.filter (f: lib.hasSuffix wheelSuffix f.url) relay.files);
in
stdenv.mkDerivation {
  pname = "icebath-relay-runtime";
  inherit (relay) version;
  src = fetchurl {
    inherit (file) url;
    sha256 = file.sha256;
  };
  dontUnpack = true;
  nativeBuildInputs = [ unzip ] ++ lib.optional stdenv.hostPlatform.isLinux autoPatchelfHook;
  buildInputs = lib.optional stdenv.hostPlatform.isLinux stdenv.cc.cc.lib;
  installPhase = ''
    runHook preInstall
    ${python.interpreter} -m venv --without-pip $out
    unzip -q $src -d $out/${python.sitePackages}
    runHook postInstall
  '';
  doInstallCheck = true;
  installCheckPhase = ''
    $out/bin/python -I -c 'import nemo_relay'
  '';
}
