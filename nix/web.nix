# Dashboard bundle compiled from web/ with the workspace lock, the tree the release binary embeds
{
  lib,
  buildNpmPackage,
  nodejs_latest,
}:
let
  root = ../.;
in
buildNpmPackage {
  pname = "icebath-web";
  version = "0";
  nodejs = nodejs_latest;
  src = lib.fileset.toSource {
    inherit root;
    fileset = lib.fileset.unions [
      ../package.json
      ../package-lock.json
      ../web
      ../apps/shared
      ../apps/desktop/package.json
      ../apps/bootstrap-installer/package.json
      ../ui-tui/package.json
      ../ui-tui/packages/hermes-ink/package.json
      ../tests-js/package.json
      ../scripts/build/web.mjs
      ../scripts/build/freshness.mjs
      ../scripts/build/frontend-common.mjs
    ];
  };
  npmDepsHash = "sha256-nLsqcdcLsw/is73PZ8o5SWLGFUAyluTEwXcemV9Alxo=";
  npmFlags = [ "--ignore-scripts" ];
  makeCacheWritable = true;
  doCheck = false;
  buildPhase = ''
    runHook preBuild
    node scripts/build/web.mjs --source "$PWD" --icons "$PWD" --out "$TMPDIR/web-product"
    runHook postBuild
  '';
  installPhase = ''
    runHook preInstall
    mkdir -p $out
    cp -r "$TMPDIR/web-product/." $out/
    runHook postInstall
  '';
}
