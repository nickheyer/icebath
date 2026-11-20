#!/bin/sh
# icebath process supervisor: one per icebath process, reaping the process groups it owns once it dies.
#
# Usage: sh -c <script> icebath-process-supervisor <parent-pgid>, spawned in its own session and stdin a
# pipe whose only write end icebath holds (pkg/tools/process_identity.go, "owner-death supervisor").
# stdin carries one command per line:
#     register <pgid>     reap this process group when icebath dies
#     unregister <pgid>   stop reaping it (icebath stopped that child itself)
# EOF means icebath is gone, by any cause including SIGKILL and the OOM killer: SIGTERM every group
# still registered, wait up to 3 s, SIGKILL the survivors, exit. A clean exit is the same EOF, so a
# child icebath never unregistered dies with it either way.
#
# POSIX sh, not Go: icebath keeps this process resident for its whole life, and an idle sh costs
# about 0.5 MB USS where a resident Go helper would cost far more. It must also never load icebath:
# it runs after icebath may already be dead.
#
# Malformed lines are skipped: a corrupt line must not cost the reaping guarantee for every other
# group. Groups that are never a valid target (pgid <= 1, our own, icebath's own) are refused at
# registration, so a caller bug costs one unreaped child, never the icebath process tree.
# Residual risk: groups are reaped by number, so a registered group that exits on its own can have
# its pgid recycled before icebath prunes it. icebath prunes empty groups on every registration change.

trap '' INT HUP  # a dying parent's SIGINT/SIGHUP must not preempt the reap: EOF is the only signal
parent=$1
# Every register line compares against it: a bad value would silently refuse every registration.
case $parent in ''|*[!0-9]*) echo "icebath process supervisor: bad parent pgid '$parent'; refusing to run" >&2; exit 2 ;; esac
# Run inside the parent's group, a killpg of the parent would take us down before we reap.
kill -0 -$$ 2>/dev/null || {
  echo "icebath process supervisor: not a process-group leader; refusing to run" >&2
  exit 2
}
reg=' '
while IFS=' ' read -r verb pg rest; do
  case $pg in ''|*[!0-9]*) continue ;; esac
  [ -z "$rest" ] && [ ${#pg} -le 10 ] || continue
  case $verb in
    register)
      [ "$pg" -gt 1 ] && [ "$pg" -ne $$ ] && [ "$pg" -ne "$parent" ] || continue
      case $reg in *" $pg "*) ;; *) reg="$reg$pg " ;; esac ;;
    unregister)
      keep=' '
      for g in $reg; do [ "$g" = "$pg" ] || keep="$keep$g "; done
      reg=$keep ;;
  esac
done
for g in $reg; do kill -TERM -"$g" 2>/dev/null; done
for _ in 1 2 3; do
  live=''
  for g in $reg; do kill -0 -"$g" 2>/dev/null && live="$live $g"; done
  reg=$live
  [ -n "$reg" ] || exit 0
  # The default PATH: a broken PATH must not turn the TERM grace into an instant KILL.
  command -p sleep 1 2>/dev/null || sleep 1
done
for g in $reg; do kill -KILL -"$g" 2>/dev/null; done
exit 0
