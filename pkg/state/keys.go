package state

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"
)

// The principal the profile scheduler admits every cron fire under
const CronPrincipalID = "cron-owner"

// Starts the admission request id of every cron fire
const CronRequestPrefix = "cron:"

var legacyCronRunID = regexp.MustCompile(`^cron_(.+)_\p{Nd}{8}_\p{Nd}{6}\n?$`)

// The admission request id of one cron fire
func CronRequestID(jobID, executionID any) string {
	return CronRequestPrefix + pyFormat(jobID) + ":" + pyFormat(executionID)
}

var errEmptyPrefix = errors.New("string index out of range")

// Returns half open bounds covering every key that starts with the prefix
func PrefixRange(prefix string) (string, string, error) {
	if prefix == "" {
		return "", "", errEmptyPrefix
	}
	last, size := utf8.DecodeLastRuneInString(prefix)
	return prefix, prefix[:len(prefix)-size] + string(last+1), nil
}

// Returns half open bounds of a job's cron request ids
func CronRequestRange(jobID any) (string, string) {
	lo, hi, _ := PrefixRange(CronRequestID(jobID, ""))
	return lo, hi
}

// Splits a cron fire request id into job and execution, a nil job meaning unknown
func CronRunFromRequest(requestID any, jobID *string) (string, string, bool) {
	request, ok := requestID.(string)
	if !ok || !strings.HasPrefix(request, CronRequestPrefix) {
		return "", "", false
	}
	var job, execution string
	if jobID == nil {
		rest := request[len(CronRequestPrefix):]
		if strings.Count(rest, ":") != 1 {
			return "", "", false
		}
		job, execution, _ = strings.Cut(rest, ":")
	} else {
		job = *jobID
		if prefix := CronRequestID(job, ""); strings.HasPrefix(request, prefix) {
			execution = request[len(prefix):]
		}
	}
	if job == "" || execution == "" {
		return "", "", false
	}
	return job, execution, true
}

// Returns the job id a pre unified cron run session names
func LegacyCronRunJob(sessionID string) (string, bool) {
	match := legacyCronRunID.FindStringSubmatch(sessionID)
	if match == nil {
		return "", false
	}
	return match[1], true
}

// Hashes the canonical target and payload after authorization and media capture
func AdmissionFingerprint(canonicalTarget string, payload map[string]any) (string, error) {
	encoded, err := pyDumps(pyObject{{"target", canonicalTarget}, {"payload", payload}},
		pyJSON{sortKeys: true, itemSep: ",", keySep: ":"})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(encoded))
	return hex.EncodeToString(sum[:]), nil
}

// The deterministic session id of a local creation receipt
func LocalIdentity(profileID, principalID, requestID any) (string, error) {
	opts := pyJSONDefault
	opts.itemSep, opts.keySep = ",", ":"
	identity, err := pyDumps([]any{profileID, principalID, requestID}, opts)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(identity))
	return "local-" + hex.EncodeToString(sum[:]), nil
}

// Maps a session key namespace slot back to its profile id
func ProfileFromSessionKeyNamespace(namespace string) string {
	if namespace == "main" {
		return "default"
	}
	if namespace == "main~" {
		return "main"
	}
	return namespace
}

// Returns the profile namespace an agent gateway session key encodes
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
