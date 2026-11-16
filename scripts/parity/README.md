# parity

Runs the reference and icebath side by side against a scripted model and diffs screens, wire requests and command output.

## Install

```sh
scripts/parity/setup.sh /tmp/ib-parity/lane1 19001
```

## Usage

```sh
cd /tmp/ib-parity/lane1
./build.sh && ./llm.sh start
python3 scripts/mkspec2.py ref ref_keys && python3 scripts/mkspec2.py ib ib_keys
ref-venv/bin/python scripts/xdrive.py cap/spec_ref_keys.json cap/x_ref_keys
ref-venv/bin/python scripts/xdrive.py cap/spec_ib_keys.json cap/x_ib_keys
ref-venv/bin/python scripts/snapdiff.py cap/x_ref_keys cap/x_ib_keys
ref-venv/bin/python scripts/battery.py scripts/battery_cmds.txt
```
