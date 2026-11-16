#!/bin/sh
# usage: setup.sh <lane-dir> <port>
set -e
HERE=$(cd "$(dirname "$0")" && pwd); REPO=$(cd "$HERE/../.." && pwd)
ROOT=$1; PORT=$2; BASE=${PARITY_BASE:-/tmp/ib-audit}
[ -n "$ROOT" ] && [ -n "$PORT" ] || { echo "usage: setup.sh <lane-dir> <port>"; exit 2; }
mkdir -p "$BASE" "$ROOT/scripts" "$ROOT/bin" "$ROOT/cap/battery" "$ROOT/dumps" "$ROOT/gotmp" "$ROOT/work/victim" "$ROOT/ref-home" "$ROOT/ib-home" "$ROOT/uh-ref" "$ROOT/uh-ib"
if [ ! -x "$BASE/ref-venv/bin/python" ]; then
  python3 -m venv "$BASE/ref-venv"
  "$BASE/ref-venv/bin/pip" install -q -r "$HERE/ref-requirements.txt" pyte
  "$BASE/ref-venv/bin/pip" install -q --no-deps browser-harness==0.1.13
fi
if [ ! -d "$BASE/xterm/node_modules/@xterm/headless" ]; then
  mkdir -p "$BASE/xterm" && (cd "$BASE/xterm" && npm init -y >/dev/null && npm install --ignore-scripts @xterm/headless >/dev/null)
fi
[ "$ROOT" = "$BASE" ] || { ln -sfn "$BASE/ref-venv" "$ROOT/ref-venv"; mkdir -p "$ROOT/xterm"; ln -sfn "$BASE/xterm/node_modules" "$ROOT/xterm/node_modules"; }
for f in "$HERE"/*.py "$HERE"/battery_cmds.txt; do sed -e "s#@ROOT@#$ROOT#g" -e "s#@PORT@#$PORT#g" "$f" > "$ROOT/scripts/$(basename "$f")"; done
sed -e "s#@ROOT@#$ROOT#g" "$HERE/bridge.js" > "$ROOT/xterm/bridge.js"
sed -e "s#@ROOT@#$ROOT#g" "$HERE/bridge2.js" > "$ROOT/xterm/bridge2.js"
for h in ref-home ib-home; do
  [ -f "$ROOT/$h/config.yaml" ] || printf 'model:\n  provider: custom\n  base_url: http://127.0.0.1:%s/v1\n  default: fake-model\n  context_length: 128000\nagent:\n  api_max_retries: 1\ntelemetry:\n  shared_metrics:\n    enabled: false\n    offer_version: 2\n    send: false\nonboarding:\n  seen:\n    busy_input_prompt: true\n    tool_progress_prompt: true\n' "$PORT" > "$ROOT/$h/config.yaml"
  [ -f "$ROOT/$h/.env" ] || printf 'OPENAI_API_KEY=sk-fake-parity\n' > "$ROOT/$h/.env"
done
printf 'alpha file\n' > "$ROOT/work/alpha.txt"; printf '# beta\n' > "$ROOT/work/beta.md"
cat > "$ROOT/ref" <<EOS
#!/bin/sh
[ -n "\$PARITY_NET" ] || { export HTTP_PROXY=http://127.0.0.1:9 HTTPS_PROXY=http://127.0.0.1:9 http_proxy=http://127.0.0.1:9 https_proxy=http://127.0.0.1:9 NO_PROXY=127.0.0.1,localhost,::1 no_proxy=127.0.0.1,localhost,::1; }
export HERMES_HOME="\${HERMES_HOME:-$ROOT/ref-home}"
export PYTHONPYCACHEPREFIX=$BASE/pycache
export PYTHONPATH=$REPO/references/projects/hermes-agent
exec $BASE/ref-venv/bin/python -m hermes_cli.main "\$@"
EOS
cat > "$ROOT/ib" <<EOS
#!/bin/sh
[ -n "\$PARITY_NET" ] || { export HTTP_PROXY=http://127.0.0.1:9 HTTPS_PROXY=http://127.0.0.1:9 http_proxy=http://127.0.0.1:9 https_proxy=http://127.0.0.1:9 NO_PROXY=127.0.0.1,localhost,::1 no_proxy=127.0.0.1,localhost,::1; }
export ICEBATH_HOME="\${ICEBATH_HOME:-$ROOT/ib-home}"
exec $ROOT/bin/icebath "\$@"
EOS
cat > "$ROOT/build.sh" <<EOS
#!/bin/sh
exec systemd-run --user --scope -q --working-directory=$REPO -p MemoryMax=\${PARITY_MEM:-14G} -p MemorySwapMax=2G env GOMEMLIMIT=\${PARITY_GOMEM:-11GiB} GOFLAGS=-p=6 GOTMPDIR=$ROOT/gotmp go build -C $REPO -tags goolm -o $ROOT/bin/icebath ./cmd/icebath
EOS
cat > "$ROOT/llm.sh" <<EOS
#!/bin/sh
case "\$1" in
  start) if [ -f $ROOT/llm.pid ] && kill -0 "\$(cat $ROOT/llm.pid)" 2>/dev/null; then exit 0; fi
    setsid $BASE/ref-venv/bin/python -I $ROOT/scripts/fakellm.py $PORT $ROOT/fakellm.log > $ROOT/fakellm.out 2>&1 < /dev/null &
    echo \$! > $ROOT/llm.pid; sleep 1 ;;
  stop) [ -f $ROOT/llm.pid ] && kill "\$(cat $ROOT/llm.pid)" 2>/dev/null; rm -f $ROOT/llm.pid ;;
esac
EOS
chmod +x "$ROOT/ref" "$ROOT/ib" "$ROOT/build.sh" "$ROOT/llm.sh"
echo "lane ready at $ROOT on port $PORT"
