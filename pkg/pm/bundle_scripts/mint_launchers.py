from __future__ import annotations

import json
import os

try:
    from distlib.scripts import ScriptMaker
except ImportError:
    from pip._vendor.distlib.scripts import ScriptMaker


def make_maker(bin_dir, shebang_python, wrapper_text):
    class MintScriptMaker(ScriptMaker):
        def __init__(self, wrapper, **kwargs):
            target_dir = kwargs.pop("target_dir")
            super().__init__(None, target_dir, **kwargs)
            self._wrapper_text = wrapper

        def _get_script_text(self, entry):
            return self._wrapper_text

    maker = MintScriptMaker(
        wrapper_text,
        target_dir=bin_dir,
        add_launchers=True,
        dry_run=False,
    )
    maker.executable = shebang_python
    return maker


def mint_one(bin_dir, shebang_python, wrapper_text, spec):
    maker = make_maker(bin_dir, shebang_python, wrapper_text)
    filenames = maker.make(f"{spec['name']} = {spec['module']}:{spec['func']}")
    plain = os.path.join(bin_dir, f"{spec['name']}.exe")
    if os.path.abspath(plain) not in (os.path.abspath(f) for f in filenames):
        raise SystemExit(f"distlib did not write {plain}: {filenames}")
    for extra in filenames:
        if os.path.abspath(extra) != os.path.abspath(plain):
            os.remove(extra)
    return plain


def main(environ=None):
    environ = os.environ if environ is None else environ
    bin_dir = environ["ICEBATH_MINT_BIN_DIR"]
    specs = json.loads(environ["ICEBATH_MINT_SPECS"])
    shebang_python = environ["ICEBATH_MINT_PYTHON"]
    if not os.path.isabs(bin_dir):
        raise SystemExit("ICEBATH_MINT_BIN_DIR must be absolute")
    if not shebang_python.startswith("<launcher_dir>\\..\\"):
        raise SystemExit(
            "ICEBATH_MINT_PYTHON must be <launcher_dir>\\..\\payload-relative "
            f"(backslashes), got: {shebang_python}"
        )
    os.makedirs(bin_dir, exist_ok=True)
    minted = []
    for spec in specs:
        with open(environ["ICEBATH_MINT_WRAPPER"], encoding="utf-8-sig") as f:
            wrapper_text = f.read()
        minted.append(mint_one(bin_dir, shebang_python, wrapper_text, spec))
    return minted


if __name__ == "__main__":
    print("\n".join(main()))
