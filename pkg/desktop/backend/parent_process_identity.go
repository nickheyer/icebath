package backend

import (
	"errors"
	"math"
	"strconv"
	"strings"
)

const maxSafeInteger = 1<<53 - 1

// Cross runtime start marker of this process from its creation time in unix milliseconds, empty for another pid
func ElectronProcessStartMarker(pid, ownPid int, creationTimeMs *float64) string {
	if pid != ownPid || creationTimeMs == nil || math.IsNaN(*creationTimeMs) || math.IsInf(*creationTimeMs, 0) {
		return ""
	}
	milliseconds := math.Trunc(*creationTimeMs)
	if milliseconds > maxSafeInteger || milliseconds <= 0 {
		return ""
	}
	return "winms:" + strconv.FormatInt(int64(milliseconds), 10)
}

// Creation time of this process in unix milliseconds, nil when unreadable
func ownCreationTimeMs() *float64 {
	ct := OwnCreateTimeSync()
	if ct == nil {
		return nil
	}
	ms := *ct * 1000
	return &ms
}

// Watchdog env naming the parent, its start marker and nonce together, a pid only watchdog when the marker is unknown
func ParentWatchdogEnv(pid int, startMarker *string, nonce string) (map[string]string, error) {
	if pid <= 0 {
		return nil, errors.New("Parent watchdog requires a positive process ID.")
	}
	env := map[string]string{"ICEBATH_PARENT_PID": strconv.Itoa(pid), "ICEBATH_SPAWN": SpawnTag(pid, startMarker)}
	if startMarker == nil {
		return env, nil
	}
	if *startMarker == "" || nonce == "" {
		return nil, errors.New("Parent watchdog marker and nonce must be non-empty.")
	}
	env["ICEBATH_PARENT_START_MARKER"] = *startMarker
	env["ICEBATH_PARENT_NONCE"] = nonce
	return env, nil
}

// Spawn tag of a desktop spawned server, its create time from a winms marker in seconds or a dash
func SpawnTag(pid int, startMarker *string) string {
	createPart := "-"
	if startMarker != nil {
		if rest, ok := strings.CutPrefix(*startMarker, "winms:"); ok {
			if ms, ok := jsNumberFloat(rest); ok && ms > 0 && !math.IsInf(ms, 0) {
				createPart = strconv.FormatFloat(ms/1000, 'f', 3, 64)
			}
		}
	}
	return "v1:-:serve:" + strconv.Itoa(pid) + ":" + createPart
}

// Number(text) as JavaScript reads it, false for NaN
func jsNumberFloat(text string) (float64, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, true
	}
	if m := jsRadixNumber.FindStringSubmatch(text); m != nil {
		base := map[byte]int{'x': 16, 'X': 16, 'o': 8, 'O': 8, 'b': 2, 'B': 2}[m[1][0]]
		n, err := strconv.ParseUint(m[2], base, 64)
		return float64(n), err == nil
	}
	switch strings.TrimPrefix(text, "+") {
	case "Infinity":
		return math.Inf(1), true
	case "-Infinity":
		return math.Inf(-1), true
	}
	if !jsDecimalNumber.MatchString(text) {
		return 0, false
	}
	f, err := strconv.ParseFloat(text, 64)
	return f, err == nil || errors.Is(err, strconv.ErrRange)
}
