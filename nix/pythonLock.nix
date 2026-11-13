# The nixpkgs Python whose major.minor matches the interpreter pkg/pm/lock.json pins for the plugin host
{
  lib,
  pkgs,
}:
let
  pythonFamily =
    lockfile:
    let
      version = (builtins.fromJSON (builtins.readFile lockfile)).packages.python.version;
      parts = builtins.match "([0-9]+)\\.([0-9]+)(\\..*)?" version;
    in
    if parts == null then
      throw "packages.python.version '${version}' is not a Python <major>.<minor> version — cannot derive the Nix interpreter from it"
    else
      builtins.elemAt parts 0 + "." + builtins.elemAt parts 1;

  selectPython =
    family: packageSet:
    let
      name = "python" + lib.replaceStrings [ "." ] [ "" ] family;
      interp = packageSet.${name} or null;
    in
    if interp == null then
      throw "package set does not provide ${name}, but pkg/pm/lock.json pins Python ${family} — update the nixpkgs input; Nix will not silently substitute another Python"
    else
      interp;

  lockfile = ../pkg/pm/lock.json;
  family = pythonFamily lockfile;
in
{
  inherit pythonFamily selectPython;

  inherit family;

  interpreter = selectPython family pkgs;
}
