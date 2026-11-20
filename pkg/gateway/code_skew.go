package gateway

import (
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/pm"
)

var (
	bootFingerprintMu sync.Mutex
	bootFingerprint   *string
)

// Checkout fingerprint of the repo the binary runs from, false when unreadable
var codeSkewFingerprint = func() (fp string, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			fp, ok = "", false
		}
	}()
	return pm.ReadGitRevisionFingerprint(pm.RepoRoot())
}

// Snapshots the checkout revision at gateway startup once
func RecordBootFingerprint() {
	bootFingerprintMu.Lock()
	defer bootFingerprintMu.Unlock()
	if bootFingerprint == nil {
		if fp, ok := codeSkewFingerprint(); ok {
			bootFingerprint = &fp
		}
	}
}

// Last fingerprint segment, cut to ten characters when it is a resolved sha
func shortFingerprint(fingerprint string) string {
	sha := fingerprint[strings.LastIndex(fingerprint, ":")+1:]
	if r := []rune(sha); sha != "" && sha != "unresolved" && len(r) > 10 {
		return string(r[:10])
	}
	if sha != "" {
		return sha
	}
	return fingerprint
}

// Full sha of the checkout on disk, false when unresolved
func CurrentCodeSHA() (string, bool) {
	fp, ok := codeSkewFingerprint()
	if !ok {
		return "", false
	}
	sha := fp[strings.LastIndex(fp, ":")+1:]
	return sha, sha != "" && sha != "unresolved"
}

// Short boot and disk revisions when the checkout drifted since boot
func DetectCodeSkew() (bootRev, diskRev string, ok bool) {
	bootFingerprintMu.Lock()
	boot := bootFingerprint
	bootFingerprintMu.Unlock()
	if boot == nil {
		return "", "", false
	}
	current, readable := codeSkewFingerprint()
	if !readable || current == *boot {
		return "", "", false
	}
	return shortFingerprint(*boot), shortFingerprint(current), true
}
