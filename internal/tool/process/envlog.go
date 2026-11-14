package process

import "strconv"

func logDeltaCommand(quotedLog string, offset int64) string {
	return "O=" + strconv.FormatInt(offset, 10) + "; " +
		"S=$({ wc -c < " + quotedLog + "; } 2>/dev/null | tr -dc '0-9'); " +
		"S=${S:-0}; " +
		`if [ "$S" -lt "$O" ]; then O=0; fi; ` +
		`N=0; P=$S; while [ "$P" -gt "$O" ] && [ "$N" -lt 3 ]; do ` +
		"B=$(tail -c +$P " + quotedLog + " 2>/dev/null | head -c 1 | od -An -to1 | tr -dc '0-9'); " +
		`case "$B" in 2[0-7][0-7]) P=$((P-1)); N=$((N+1));; *) break;; esac; done; ` +
		`if [ "$N" -gt 0 ] || [ "$P" -eq "$S" ]; then ` +
		"B=$(tail -c +$P " + quotedLog + " 2>/dev/null | head -c 1 | od -An -to1 | tr -dc '0-9'); " +
		`case "$B" in 3[0-3][0-7]) L=2;; 3[4-5][0-7]) L=3;; 3[6-7][0-7]) L=4;; *) L=1;; esac; ` +
		`if [ "$L" -gt $((N+1)) ]; then S=$((P-1)); fi; fi; ` +
		`echo "$S $O"; ` +
		`if [ "$S" -gt "$O" ]; then ` +
		"tail -c +$((O+1)) " + quotedLog + " 2>/dev/null | head -c $((S-O)); fi"
}
