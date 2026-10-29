"""Builds one icebath-cli wheel per platform from the icebath release binaries"""

import base64
import hashlib
import sys
import zipfile
from pathlib import Path

NAME = "icebath_cli"
PLATFORMS = {
    ("linux", "amd64"): "manylinux_2_17_x86_64.manylinux2014_x86_64.musllinux_1_1_x86_64",
    ("linux", "arm64"): "manylinux_2_17_aarch64.manylinux2014_aarch64.musllinux_1_1_aarch64",
    ("darwin", "amd64"): "macosx_13_0_x86_64",
    ("darwin", "arm64"): "macosx_13_0_arm64",
    ("windows", "amd64"): "win_amd64",
    ("windows", "arm64"): "win_arm64",
}
SUMMARY = "Tiling agentic harness and netrunner toolkit"
DESCRIPTION = """# icebath

A tiling agentic harness and netrunner toolkit for a deep dive beyond the blackwall.

```sh
pipx install icebath-cli
icebath
```

See https://icebath.app for every other way to install it.
"""


# Returns the sha256 RECORD digest of some bytes
def digest(data):
    return "sha256=" + base64.urlsafe_b64encode(hashlib.sha256(data).digest()).rstrip(b"=").decode()


# Writes one wheel holding the binary as an installed script
def wheel(version, binary, exe, tag, license_text, out):
    info = f"{NAME}-{version}.dist-info"
    metadata = "\n".join([
        "Metadata-Version: 2.4",
        "Name: icebath-cli",
        f"Version: {version}",
        f"Summary: {SUMMARY}",
        "Author-email: Nicholas W. Heyer <nick@heyer.app>",
        "License-Expression: MIT",
        "License-File: LICENSE",
        "Project-URL: Homepage, https://icebath.app",
        "Project-URL: Source, https://github.com/nickheyer/icebath",
        "Description-Content-Type: text/markdown",
        "",
        DESCRIPTION,
    ])
    tags = [f"py3-none-{t}" for t in tag.split(".")]
    wheel_file = "\n".join(["Wheel-Version: 1.0", "Generator: icebath", "Root-Is-Purelib: false", *[f"Tag: {t}" for t in tags], ""])
    files = [
        (f"{NAME}-{version}.data/scripts/icebath{exe}", binary.read_bytes(), 0o755),
        (f"{info}/METADATA", metadata.encode(), 0o644),
        (f"{info}/WHEEL", wheel_file.encode(), 0o644),
        (f"{info}/licenses/LICENSE", license_text, 0o644),
    ]
    record = "".join(f"{n},{digest(d)},{len(d)}\n" for n, d, _ in files) + f"{info}/RECORD,,\n"
    files.append((f"{info}/RECORD", record.encode(), 0o644))
    path = out / f"{NAME}-{version}-py3-none-{tag}.whl"
    with zipfile.ZipFile(path, "w", zipfile.ZIP_DEFLATED) as zf:
        for name, data, mode in files:
            entry = zipfile.ZipInfo(name, date_time=(1980, 1, 1, 0, 0, 0))
            entry.external_attr = (0o100000 | mode) << 16
            entry.compress_type = zipfile.ZIP_DEFLATED
            zf.writestr(entry, data)
    return path


# Writes a wheel for every platform from release binaries named icebath-os-arch
def main(version, binaries, out, license_path):
    binaries, out = Path(binaries), Path(out)
    out.mkdir(parents=True, exist_ok=True)
    license_text = Path(license_path).read_bytes()
    for (goos, goarch), tag in PLATFORMS.items():
        exe = ".exe" if goos == "windows" else ""
        binary = binaries / f"icebath-{goos}-{goarch}{exe}"
        if not binary.is_file():
            sys.exit(f"wheels: missing {binary}")
        print(wheel(version, binary, exe, tag, license_text, out))


if __name__ == "__main__":
    if len(sys.argv) != 5:
        sys.exit("usage: wheels.py <version> <binaries dir> <out dir> <license file>")
    main(*sys.argv[1:])
