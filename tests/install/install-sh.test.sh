#!/bin/sh
# Headless cases of scripts/install.sh against a file:// release with a recording icebath binary
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/../../scripts/install.sh"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/icebath-install-test-XXXXXX")"
trap 'rm -rf "$WORK"' EXIT
PASS=0
FAIL=0

case "$(uname -s)" in Linux) os=linux ;; Darwin) os=darwin ;; esac
case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; esac
VERSION=9.8.7
REL="$WORK/repo/releases/download/v$VERSION"
mkdir -p "$REL" "$WORK/pkg"
cat > "$WORK/pkg/icebath" <<'BIN'
#!/bin/sh
printf '%s\n' "$@" > "$ICEBATH_TEST_ARGS"
printf '%s\n' "$ICEBATH_HOME" > "$ICEBATH_TEST_ARGS.home"
BIN
chmod +x "$WORK/pkg/icebath"
archive="icebath_${VERSION}_${os}_${arch}.tar.gz"
tar -czf "$REL/$archive" -C "$WORK/pkg" icebath
if command -v sha256sum >/dev/null 2>&1; then sum="$(sha256sum "$REL/$archive" | cut -d' ' -f1)"; else sum="$(shasum -a 256 "$REL/$archive" | cut -d' ' -f1)"; fi
printf '%s  %s\n' "$sum" "$archive" > "$REL/checksums.txt"

run() {
    ICEBATH_REPO_URL="file://$WORK/repo" ICEBATH_VERSION="v$VERSION" ICEBATH_HOME="$WORK/home" \
        ICEBATH_TEST_ARGS="$WORK/args" bash "$SCRIPT" "$@"
}

check() {
    name="$1"; shift
    if "$@"; then PASS=$((PASS + 1)); echo "PASS $name"; else FAIL=$((FAIL + 1)); echo "FAIL $name"; fi
}

forwards_flags_to_install_self() {
    rm -f "$WORK/args"
    run --channel main --branch dev --commit abc123 --stage products --json --non-interactive \
        --include-desktop --skip-browser --skip-computer-use --verbose >/dev/null 2>&1 || return 1
    got="$(tr '\n' ' ' < "$WORK/args")"
    want="install-self --dir $WORK/home/icebath-agent --icebath-home $WORK/home --version $VERSION --branch dev --commit abc123 --stage products --json --non-interactive --include-desktop --skip-browser --skip-computer-use --verbose --channel main "
    [ "$got" = "$want" ] || { echo "  got:  $got"; echo "  want: $want"; return 1; }
    [ "$(cat "$WORK/args.home")" = "$WORK/home" ]
}

manifest_is_served_by_the_binary() {
    rm -f "$WORK/args"
    run --manifest >/dev/null 2>&1 && grep -qx -- --manifest "$WORK/args"
}

dir_and_home_flags_override_defaults() {
    rm -f "$WORK/args"
    run --dir "$WORK/custom" --icebath-home "$WORK/other" >/dev/null 2>&1 || return 1
    got="$(head -n 5 "$WORK/args" | tr '\n' ' ')"
    [ "$got" = "install-self --dir $WORK/custom --icebath-home $WORK/other " ]
}

checksum_mismatch_is_refused() {
    cp "$REL/checksums.txt" "$WORK/sums"
    printf '%s  %s\n' 0000000000000000000000000000000000000000000000000000000000000000 "$archive" > "$REL/checksums.txt"
    rm -f "$WORK/args"
    out="$(run 2>&1)"; rc=$?
    cp "$WORK/sums" "$REL/checksums.txt"
    [ "$rc" = 1 ] && echo "$out" | grep -q "checksum mismatch" && [ ! -e "$WORK/args" ]
}

invalid_channel_is_refused() {
    out="$(run --channel nightly 2>&1)"; rc=$?
    [ "$rc" = 2 ] && echo "$out" | grep -q -- "--channel must be stable or main, not nightly"
}

value_flags_need_a_value() {
    out="$(run --stage 2>&1)"; rc=$?
    [ "$rc" = 2 ] && echo "$out" | grep -q -- "--stage needs a value"
}

unknown_option_is_refused() {
    out="$(run --bogus 2>&1)"; rc=$?
    [ "$rc" = 1 ] && echo "$out" | grep -q "unknown option: --bogus"
}

help_prints_usage() {
    out="$(run --help 2>&1)"; rc=$?
    [ "$rc" = 0 ] && echo "$out" | grep -q "Usage: install.sh"
}

check forwards_flags_to_install_self forwards_flags_to_install_self
check manifest_is_served_by_the_binary manifest_is_served_by_the_binary
check dir_and_home_flags_override_defaults dir_and_home_flags_override_defaults
check checksum_mismatch_is_refused checksum_mismatch_is_refused
check invalid_channel_is_refused invalid_channel_is_refused
check value_flags_need_a_value value_flags_need_a_value
check unknown_option_is_refused unknown_option_is_refused
check help_prints_usage help_prints_usage
echo "$PASS passed, $FAIL failed"
[ "$FAIL" = 0 ]
