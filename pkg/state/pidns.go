package state

import (
	"os"
	"regexp"
	"runtime"
	"sync"
)

var pidnsTokenRE = regexp.MustCompile(`(?:^|:)pidns=(\p{Nd}+)(?::|$)`)

var pidnsLinkRE = regexp.MustCompile(`\[(\p{Nd}+)\]`)

// Own PID namespace identity, an empty ID on a supported platform meaning the lookup failed
type LocalPidNamespace struct {
	ID        string
	Supported bool
}

var (
	localPidNSMu sync.Mutex
	localPidNS   *LocalPidNamespace
)

// Reads this process' PID namespace inode, unsupported off Linux and unresolved when the link cannot be read
var resolveLocalPidNamespace = func() LocalPidNamespace {
	if runtime.GOOS != "linux" {
		return LocalPidNamespace{}
	}
	link, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return LocalPidNamespace{Supported: true}
	}
	if match := pidnsLinkRE.FindStringSubmatch(link); match != nil {
		return LocalPidNamespace{ID: match[1], Supported: true}
	}
	return LocalPidNamespace{Supported: true}
}

// This process' PID namespace identity, cached only once the answer is definite
func localPidNamespace() LocalPidNamespace {
	localPidNSMu.Lock()
	defer localPidNSMu.Unlock()
	if localPidNS != nil {
		return *localPidNS
	}
	local := resolveLocalPidNamespace()
	if local.ID != "" || !local.Supported {
		localPidNS = &local
	}
	return local
}

// The namespace inode a holder records, empty when there is none or the lookup failed
func PidNamespaceID() string {
	return localPidNamespace().ID
}

// The pidns fragment appended to a new holder string, empty without a namespace
func HolderNamespaceToken() string {
	if ns := PidNamespaceID(); ns != "" {
		return ":pidns=" + ns
	}
	return ""
}

// The pidns stamp of a holder string, false when it carries none
func recordedNamespace(holder string) (string, bool) {
	match := pidnsTokenRE.FindStringSubmatch(holder)
	if match == nil {
		return "", false
	}
	return match[1], true
}

// Whether a pid recorded in a namespace may be probed here, unstamped records following the given policy
func qualifyPidNamespace(recorded string, stamped, unstampedCheckable bool) bool {
	local := localPidNamespace()
	if !local.Supported {
		return true
	}
	if !stamped {
		return unstampedCheckable
	}
	if local.ID == "" {
		return false
	}
	return recorded == local.ID
}

// Reports whether a TTL holder's pid may be probed, foreign and unstamped holders deferring to expiry
func HolderPidCheckable(holder string) bool {
	recorded, stamped := recordedNamespace(holder)
	return qualifyPidNamespace(recorded, stamped, false)
}

// Reports whether a no expiry record may be probed, unstamped records still probing
func PersistentRecordPidnsCheckable(recorded string, stamped bool) bool {
	return qualifyPidNamespace(recorded, stamped, true)
}
