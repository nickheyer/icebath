package state

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
)

// Prefix of the admission request id of one cron fire
const CronRequestPrefix = "cron:"

// Session id a pre-unified cron run was named by
var legacyCronRunID = regexp.MustCompile(`^cron_(.+)_\p{Nd}{8}_\p{Nd}{6}\n?$`)

// Admission request id of one cron fire, executionID being its ledger row id
func CronRequestID(jobID, executionID string) string {
	return CronRequestPrefix + jobID + ":" + executionID
}

// Half open bounds of every key starting with prefix, false when no key can bound it from above
func PrefixRange(prefix string) (string, string, bool) {
	last, size := utf8.DecodeLastRuneInString(prefix)
	if size == 0 || !utf8.ValidString(prefix) || !utf8.ValidRune(last+1) {
		return "", "", false
	}
	return prefix, prefix[:len(prefix)-size] + string(last+1), true
}

// Half open bounds of the cron request ids of jobID
func CronRequestRange(jobID string) (string, string) {
	prefix := CronRequestID(jobID, "")
	return prefix, prefix[:len(prefix)-1] + ";"
}

// Job and execution of a cron request id, a policy named jobID splitting an ambiguous one, false otherwise
func CronRunFromRequest(requestID, jobID string) (string, string, bool) {
	rest, ok := strings.CutPrefix(requestID, CronRequestPrefix)
	if !ok {
		return "", "", false
	}
	var execution string
	if jobID == "" {
		if strings.Count(rest, ":") != 1 {
			return "", "", false
		}
		jobID, execution, _ = strings.Cut(rest, ":")
	} else if prefix := CronRequestID(jobID, ""); strings.HasPrefix(requestID, prefix) {
		execution = requestID[len(prefix):]
	}
	if jobID == "" || execution == "" {
		return "", "", false
	}
	return jobID, execution, true
}

// Job id a pre-unified cron run session names, false for any other id
func LegacyCronRunJob(sessionID string) (string, bool) {
	m := legacyCronRunID.FindStringSubmatch(sessionID)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// Canonical payload identity of an admission after authorization and media capture
func AdmissionFingerprint(canonicalTarget string, payload any) (string, error) {
	encoded, err := fileutil.EncodeJSON(map[string]any{"target": canonicalTarget, "payload": payload}, fileutil.JSONStyle{Tight: true, Sorted: true})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// Deterministic session id of a local creation receipt
func LocalIdentity(profileID, principalID, requestID string) (string, error) {
	encoded, err := fileutil.EncodeJSON([]string{profileID, principalID, requestID}, fileutil.JSONStyle{Tight: true, ASCII: true})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return "local-" + hex.EncodeToString(sum[:]), nil
}

// Profile a namespace slot of a gateway session key names, default for main and main for the marked main~
func ProfileFromSessionKeyNamespace(namespace string) string {
	switch namespace {
	case "main":
		return home.DefaultProfile
	case "main~":
		return "main"
	}
	return namespace
}

// Profile namespace an agent gateway session key encodes, false for any other key
func ProfileFromSessionKey(sessionKey string) (string, bool) {
	if sessionKey == "" {
		return "", false
	}
	parts := strings.Split(sessionKey, ":")
	if len(parts) < 2 || parts[0] != "agent" {
		return "", false
	}
	ns := parts[1]
	if ns == "" {
		ns = "main"
	}
	return ProfileFromSessionKeyNamespace(ns), true
}
