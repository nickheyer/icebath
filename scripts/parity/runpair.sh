#!/bin/sh
# usage: runpair.sh <tag> <home-suffix> <args...>   runs ib then ref with homes/<suffix>-ib and homes/<suffix>-ref, dumping wire requests
L=@ROOT@
tag="$1"; suf="$2"; shift 2
echo "${tag}_ib" > $L/dump_tag
HOME=$L/uh-ib ICEBATH_HOME=$L/homes/$suf-ib timeout ${PAIR_TIMEOUT:-90} $L/ib "$@" > $L/cap/${tag}.ib.txt 2>&1; echo "ib exit $?"
echo "${tag}_ref" > $L/dump_tag
HOME=$L/uh-ref HERMES_HOME=$L/homes/$suf-ref timeout ${PAIR_TIMEOUT:-90} $L/ref "$@" > $L/cap/${tag}.ref.txt 2>&1; echo "ref exit $?"
rm -f $L/dump_tag
ls $L/dumps | grep "^${tag}_" | tr '\n' ' '; echo
