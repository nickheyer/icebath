package state

import (
	"errors"
	"os"
	"regexp"
	"strconv"
	"sync"

	"github.com/nickheyer/icebath/internal/proc"
)

// Namespace token inside a structured holder string
var pidnsToken = regexp.MustCompile(`(?:^|:)pidns=(\d+)(?::|$)`)

// pid token inside a structured holder string
var pidToken = regexp.MustCompile(`(?:^|:)pid=(\d+)(?::|$)`)

// What this process knows of its own pid namespace, kept once definite since a process never changes namespace
type pidNamespace struct {
	mu        sync.Mutex
	resolved  bool
	id        string
	supported bool
}

// Identity of the local pid namespace and whether the platform has namespaces, an empty id when the lookup failed
func (e *Engine) pidNamespace() (string, bool) {
	n := &e.pidns
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.resolved {
		return n.id, n.supported
	}
	id, err := proc.PIDNamespace()
	switch {
	case errors.Is(err, errors.ErrUnsupported):
		n.resolved, n.supported = true, false
	case err == nil:
		n.resolved, n.supported, n.id = true, true, id
	default:
		return "", true
	}
	return n.id, n.supported
}

// Fragment a new holder string appends to record its pid namespace, empty where none can be proven
func (e *Engine) holderNamespaceToken() string {
	if id, _ := e.pidNamespace(); id != "" {
		return ":pidns=" + id
	}
	return ""
}

// Whether a recorded pid may be probed here, unstampedOK deciding for records that name no namespace
func (e *Engine) pidCheckable(recorded string, stamped, unstampedOK bool) bool {
	id, supported := e.pidNamespace()
	switch {
	case !supported:
		return true
	case !stamped:
		return unstampedOK
	case id == "":
		return false
	}
	return recorded == id
}

// Strict policy for expiring rows, a foreign or unstamped namespace deferring to the row expiry instead of a probe
func (e *Engine) holderPIDCheckable(holder string) bool {
	m := pidnsToken.FindStringSubmatch(holder)
	return e.pidCheckable(matchGroup(m), m != nil, false)
}

// Legacy policy for records without expiry, an unstamped record still probed so orphan cleanup keeps working
func (e *Engine) persistentRecordCheckable(recorded string) bool {
	return e.pidCheckable(recorded, recorded != "", true)
}

// First submatch of m, empty when nothing matched
func matchGroup(m []string) string {
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// Reports a lock holder whose local pid is provably gone, any doubt or foreign namespace keeping the lease until expiry
func (e *Engine) holderProcessGone(holder string) bool {
	pid, err := strconv.Atoi(matchGroup(pidToken.FindStringSubmatch(holder)))
	if err != nil || pid <= 0 || pid == os.Getpid() || !e.holderPIDCheckable(holder) {
		return false
	}
	return !proc.Exists(pid)
}
