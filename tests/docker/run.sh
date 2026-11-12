#!/bin/sh
# Container contract suite: runs every case against IMAGE (default icebath:test), RUN filters case names by substring
set -u
IMAGE="${IMAGE:-icebath:test}"
RUN="${RUN:-}"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/icebath-docker-XXXXXX")"
PASS=0
FAIL=0
FAILED=""
CONTAINERS=""

cleanup() {
    for c in $CONTAINERS; do docker rm -f "$c" >/dev/null 2>&1; done
    docker run --rm -v "$WORK:/w" --entrypoint /bin/rm "$IMAGE" -rf /w/vols >/dev/null 2>&1
    rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

fail() { echo "    $*"; return 1; }

# Fresh host directory mounted as /opt/data
vol() {
    d="$WORK/vols/$1"
    mkdir -p "$d"
    printf '%s' "$d"
}

# Detached container with a unique name, recorded for cleanup
start() {
    name="icebath-test-$$-$1"; shift
    CONTAINERS="$CONTAINERS $name"
    docker run -d --name "$name" "$@" >/dev/null || return 1
    printf '%s' "$name"
}

# Polls cmd inside container c until it prints want or the timeout passes
wait_for() {
    c="$1"; want="$2"; secs="$3"; shift 3
    i=0
    while [ "$i" -lt "$((secs * 4))" ]; do
        out="$(docker exec "$c" "$@" 2>/dev/null)"
        [ "$out" = "$want" ] && return 0
        sleep 0.25
        i=$((i + 1))
    done
    fail "waited ${secs}s for '$want' from '$*', last output '$out'"
}

# Waits until the boot reached the point where services start
booted() { wait_for "$1" yes 60 sh -c 'test -f /run/service/dashboard/supervise/status.json && echo yes'; }

owner() { docker run --rm -v "$1:/v" --entrypoint stat "$IMAGE" -c %u "/v/$2"; }

test_icebath_help() {
    out="$(docker run --rm "$IMAGE" --help 2>&1)" || fail "exit $?" || return 1
    echo "$out" | grep -qi usage || fail "no usage text: $out"
}

test_dashboard_subcommand_present() {
    docker run --rm "$IMAGE" dashboard --help >/dev/null 2>&1 || fail "dashboard --help failed"
}

test_icebath_help_under_wrapped_init() {
    docker run --rm --init "$IMAGE" --help >/dev/null 2>&1 || fail "--help under --init failed"
}

test_legacy_tini_entrypoint_boots() {
    docker run --rm --entrypoint /usr/bin/tini "$IMAGE" -g -- --version >/dev/null 2>&1 || fail "tini shim did not hand over to /init"
}

test_bash_pattern() {
    out="$(docker run --rm "$IMAGE" bash -c 'echo hello-from-bash' 2>/dev/null)"
    echo "$out" | grep -qx hello-from-bash || fail "got: $out"
}

test_container_exit_code_matches_inner_exit() {
    docker run --rm "$IMAGE" sh -c 'exit 42' >/dev/null 2>&1
    rc=$?
    [ "$rc" = 42 ] || fail "exit $rc, want 42"
}

test_main_program_runs_as_icebath() {
    out="$(docker run --rm "$IMAGE" sh -c 'id -un; echo $HOME' 2>/dev/null | tr '\n' ' ')"
    [ "$out" = "icebath /opt/data " ] || fail "got: $out"
}

test_docker_image_contains_license_file() {
    docker run --rm "$IMAGE" test -s /opt/icebath/LICENSE || fail "LICENSE missing"
}

test_container_sets_hosted_write_policy_env() {
    out="$(docker run --rm "$IMAGE" sh -c 'echo "$ICEBATH_HOME $ICEBATH_WRITE_SAFE_ROOT"' 2>/dev/null)"
    [ "$out" = "/opt/data /opt/data" ] || fail "got: $out"
}

test_login_shell_resolves_icebath_to_the_image_binary() {
    out="$(docker run --rm "$IMAGE" bash -lc 'command -v icebath' 2>/dev/null)"
    [ "$out" = /opt/icebath/icebath ] || fail "got: $out"
}

test_install_tree_not_writable_by_icebath() {
    docker run --rm "$IMAGE" sh -c 'touch /opt/icebath/x' >/dev/null 2>&1 && fail "install tree writable"
    return 0
}

test_icebath_user_cannot_modify_install_but_can_write_data() {
    v="$(vol data-write)"
    docker run --rm -v "$v:/opt/data" "$IMAGE" sh -c 'touch /opt/data/ok && ! touch /opt/icebath/icebath' >/dev/null 2>&1 \
        || fail "data not writable or install writable"
}

test_logs_gateways_seeded_and_icebath_owned() {
    v="$(vol logs-seed)"
    docker run --rm -v "$v:/opt/data" "$IMAGE" true >/dev/null 2>&1
    [ "$(owner "$v" logs/gateways)" = 10000 ] || fail "logs/gateways not owned by 10000"
}

test_seeds_config_env_and_soul_on_first_boot() {
    v="$(vol seed)"
    docker run --rm -v "$v:/opt/data" "$IMAGE" true >/dev/null 2>&1
    for f in .env config.yaml SOUL.md; do
        [ "$(owner "$v" "$f")" = 10000 ] || { fail "$f missing or not owned by 10000"; return 1; }
    done
    docker run --rm -v "$v:/v" --entrypoint grep "$IMAGE" -q '^API_SERVER_KEY=..*' /v/.env || fail "no API_SERVER_KEY generated"
}

test_stale_docker_stamp_in_home_is_healed_on_boot() {
    v="$(vol stamp)"
    printf 'docker\n' > "$v/.install_method"
    docker run --rm -v "$v:/opt/data" "$IMAGE" true >/dev/null 2>&1
    [ ! -e "$v/.install_method" ] || fail ".install_method docker stamp kept"
}

test_root_owned_state_files_repaired_on_boot() {
    v="$(vol repair)"
    docker run --rm -v "$v:/v" --entrypoint sh "$IMAGE" -c 'echo {} > /v/auth.json; mkdir -p /v/cron; touch /v/cron/jobs.json' >/dev/null
    docker run --rm -v "$v:/opt/data" "$IMAGE" true >/dev/null 2>&1
    [ "$(owner "$v" auth.json)" = 10000 ] || fail "auth.json not repaired"
    [ "$(owner "$v" cron/jobs.json)" = 10000 ] || fail "cron tree not repaired"
}

test_non_allowlisted_host_file_not_touched() {
    v="$(vol allowlist)"
    docker run --rm -v "$v:/v" --entrypoint sh "$IMAGE" -c 'echo x > /v/notes.txt' >/dev/null
    docker run --rm -v "$v:/opt/data" "$IMAGE" true >/dev/null 2>&1
    [ "$(owner "$v" notes.txt)" = 0 ] || fail "non allowlisted file chowned"
}

test_symlinked_allowlisted_file_not_chowned() {
    v="$(vol symlink)"
    docker run --rm -v "$v:/v" --entrypoint sh "$IMAGE" -c 'echo x > /v/target; ln -s /opt/data/target /v/auth.json' >/dev/null
    out="$(docker run --rm -v "$v:/opt/data" "$IMAGE" true 2>&1)"
    [ "$(owner "$v" target)" = 0 ] || fail "symlink target chowned"
    echo "$out" | grep -q "refusing chown through symlinked path" || fail "no refusal warning: $out"
}

test_seeds_running_state_on_blank_volume() {
    v="$(vol state-seed)"
    docker run --rm -v "$v:/opt/data" -e ICEBATH_GATEWAY_BOOTSTRAP_STATE=running "$IMAGE" true >/dev/null 2>&1
    [ "$(cat "$v/gateway_state.json" 2>/dev/null)" = '{"gateway_state":"running"}' ] || fail "state not seeded"
}

test_no_seed_when_env_unset() {
    v="$(vol state-none)"
    docker run --rm -v "$v:/opt/data" "$IMAGE" true >/dev/null 2>&1
    [ ! -e "$v/gateway_state.json" ] || fail "state seeded without the variable"
}

test_non_running_value_ignored() {
    v="$(vol state-stopped)"
    docker run --rm -v "$v:/opt/data" -e ICEBATH_GATEWAY_BOOTSTRAP_STATE=stopped "$IMAGE" true >/dev/null 2>&1
    [ ! -e "$v/gateway_state.json" ] || fail "state seeded for a non running value"
}

test_does_not_seed_gateway_state_through_symlink() {
    v="$(vol state-link)"
    docker run --rm -v "$v:/v" --entrypoint sh "$IMAGE" -c 'ln -s /opt/data/elsewhere /v/gateway_state.json' >/dev/null
    docker run --rm -v "$v:/opt/data" -e ICEBATH_GATEWAY_BOOTSTRAP_STATE=running "$IMAGE" true >/dev/null 2>&1
    [ ! -e "$v/elsewhere" ] || fail "state written through the symlink"
}

test_arbitrary_user_uid_rejected() {
    out="$(docker run --rm --user 12345 "$IMAGE" --version 2>&1)"
    rc=$?
    [ "$rc" = 1 ] || { fail "exit $rc, want 1"; return 1; }
    echo "$out" | grep -q "not supported" || fail "no guidance: $out"
}

test_user_pinned_to_icebath_uid_works() {
    docker run --rm --user 10000 "$IMAGE" sh -c 'exit 0' >/dev/null 2>&1 || fail "--user 10000 refused"
}

test_puid_pgid_remaps_icebath_user() {
    v="$(vol remap)"
    out="$(docker run --rm -v "$v:/opt/data" -e ICEBATH_UID=4321 -e ICEBATH_GID=4322 "$IMAGE" sh -c 'id -u; id -g; touch /opt/data/made' 2>/dev/null | tr '\n' ' ')"
    [ "$out" = "4321 4322 " ] || { fail "ids: $out"; return 1; }
    [ "$(owner "$v" made)" = 4321 ] || fail "volume file not owned by the remapped uid"
}

test_nas_low_uid_accepted() {
    out="$(docker run --rm -e PUID=1026 -e PGID=100 "$IMAGE" sh -c 'id -u; id -g' 2>/dev/null | tr '\n' ' ')"
    [ "$out" = "1026 100 " ] || fail "ids: $out"
}

test_shim_drops_root_to_icebath_uid() {
    v="$(vol shim)"
    c="$(start shim -v "$v:/opt/data" "$IMAGE" sleep infinity)" || return 1
    booted "$c" || return 1
    docker exec "$c" icebath config set model.default shim-test >/dev/null 2>&1 || { fail "config set failed"; return 1; }
    [ "$(owner "$v" config.yaml)" = 10000 ] || fail "docker exec wrote config.yaml as root"
    out="$(docker exec -e ICEBATH_DOCKER_EXEC_AS_ROOT=1 "$c" sh -c 'id -u')"
    [ "$out" = 0 ] || fail "plain commands of docker exec should stay root"
}

test_orphan_zombies_reaped() {
    out="$(docker run --rm "$IMAGE" sh -c '(sh -c "sleep 1 &" &); sleep 3; ps -eo stat= | grep -c "^Z"' 2>/dev/null)"
    [ "$out" = 0 ] || fail "zombies: $out"
}

test_dashboard_not_running_by_default() {
    c="$(start dash-off "$IMAGE" sleep infinity)" || return 1
    booted "$c" || return 1
    out="$(docker exec "$c" s6-svstat -o up,wantedup /run/service/dashboard)"
    [ "$out" = "false false" ] || fail "dashboard state: $out"
}

test_dashboard_supervised_when_env_set() {
    v="$(vol dash-on)"
    c="$(start dash-on -v "$v:/opt/data" -e ICEBATH_DASHBOARD=1 -e ICEBATH_DASHBOARD_HOST=127.0.0.1 "$IMAGE" sleep infinity)" || return 1
    booted "$c" || return 1
    wait_for "$c" "true true" 90 s6-svstat -o up,ready /run/service/dashboard || return 1
    docker exec "$c" curl -fsS -o /dev/null http://127.0.0.1:9119/ || fail "dashboard did not answer"
    docker exec "$c" icebath container-health || fail "healthcheck failed with the dashboard ready"
}

test_dashboard_service_resets_home() {
    v="$(vol dash-home)"
    c="$(start dash-home -v "$v:/opt/data" -e ICEBATH_DASHBOARD=1 -e ICEBATH_DASHBOARD_HOST=127.0.0.1 -e HOME=/root "$IMAGE" sleep infinity)" || return 1
    booted "$c" || return 1
    wait_for "$c" true 60 s6-svstat -o up /run/service/dashboard || return 1
    pid="$(docker exec "$c" s6-svstat -o pid /run/service/dashboard)"
    docker exec "$c" sh -c "tr '\\0' '\\n' < /proc/$pid/environ | grep -qx HOME=/opt/data" || fail "dashboard HOME not /opt/data"
}

test_dashboard_insecure_env_var_no_longer_bypasses_gate() {
    v="$(vol dash-insecure)"
    c="$(start dash-insecure -v "$v:/opt/data" -e ICEBATH_DASHBOARD=1 -e ICEBATH_DASHBOARD_INSECURE=1 "$IMAGE" sleep infinity)" || return 1
    booted "$c" || return 1
    i=0
    while [ $i -lt 120 ]; do
        docker logs "$c" 2>&1 | grep -q "ICEBATH_DASHBOARD_INSECURE no longer disables the auth gate" && return 0
        sleep 0.5; i=$((i + 1))
    done
    fail "no insecure warning"
}

test_s6_register_creates_service_dir_in_live_container() {
    v="$(vol register)"
    c="$(start register -v "$v:/opt/data" "$IMAGE" sleep infinity)" || return 1
    booted "$c" || return 1
    docker exec "$c" icebath profile create worker >/dev/null 2>&1 || { fail "profile create failed"; return 1; }
    wait_for "$c" yes 20 sh -c 'test -x /run/service/gateway-worker/run && echo yes' || return 1
    wait_for "$c" false 20 s6-svstat -o up /run/service/gateway-worker
}

test_reconcile_registers_default_slot() {
    v="$(vol reconcile)"
    c="$(start reconcile -v "$v:/opt/data" "$IMAGE" sleep infinity)" || return 1
    booted "$c" || return 1
    docker exec "$c" test -x /run/service/gateway-default/run || { fail "no gateway-default slot"; return 1; }
    grep -q "profile=default prior_state=None action=registered" "$v/logs/container-boot.log" || fail "reconcile log missing"
}

test_gateway_run_redirects_to_supervised() {
    v="$(vol supervised)"
    c="$(start supervised -v "$v:/opt/data" "$IMAGE" gateway run)" || return 1
    booted "$c" || return 1
    grep -q '"migrated_from": "legacy-container-cmd"' "$v/gateway_state.json" || { fail "legacy gateway run state not seeded"; return 1; }
    wait_for "$c" true 60 s6-svstat -o wantedup /run/service/gateway-default
}

test_stage2_discovers_chromium_binary() {
    out="$(docker run --rm "$IMAGE" true 2>&1)"
    echo "$out" | grep -q "\[stage2\] Using pinned Chromium binary:" || fail "no Chromium discovery line: $out"
}

test_stale_gateway_pid_cleaned_up_on_restart() {
    v="$(vol stale-pid)"
    docker run --rm -v "$v:/opt/data" "$IMAGE" true >/dev/null 2>&1
    docker run --rm -v "$v:/v" --entrypoint sh "$IMAGE" -c 'echo 99999 > /v/gateway.pid; echo {} > /v/processes.json' >/dev/null
    docker run --rm -v "$v:/opt/data" "$IMAGE" true >/dev/null 2>&1
    [ ! -e "$v/gateway.pid" ] && [ ! -e "$v/processes.json" ] || fail "stale runtime files kept"
}

test_stopped_gateway_stays_stopped_after_restart() {
    v="$(vol stopped)"
    docker run --rm -v "$v:/opt/data" "$IMAGE" true >/dev/null 2>&1
    printf '{"gateway_state": "stopped", "desired_state": "stopped"}\n' > "$v/gateway_state.json"
    c="$(start stopped -v "$v:/opt/data" "$IMAGE" sleep infinity)" || return 1
    booted "$c" || return 1
    out="$(docker exec "$c" s6-svstat -o up,wantedup /run/service/gateway-default)"
    [ "$out" = "false false" ] || fail "stopped gateway state: $out"
}

test_healthcheck_reports_healthy_when_idle() {
    c="$(start health "$IMAGE" sleep infinity)" || return 1
    booted "$c" || return 1
    docker exec "$c" icebath container-health || fail "idle container unhealthy"
}

for t in $(grep -o '^test_[a-z0-9_]*' "$0"); do
    case "$t" in *"$RUN"*) ;; *) continue ;; esac
    if out="$($t 2>&1)"; then
        PASS=$((PASS + 1))
        echo "PASS $t"
    else
        FAIL=$((FAIL + 1))
        FAILED="$FAILED $t"
        echo "FAIL $t"
        [ -n "$out" ] && echo "$out"
    fi
done
echo "$PASS passed, $FAIL failed"
[ "$FAIL" = 0 ]
