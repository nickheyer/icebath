package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/tools"
)

// ForeignGatewayPid is a gateway pid outside the update scope with its home, empty when unreadable
type ForeignGatewayPid struct {
	PID  int
	Home string
}

// Expanded and resolved form of a home path, false when it cannot be resolved
func resolvedScopeHome(p string) (string, bool) {
	expanded := pypath.Expanduser(p)
	resolved, err := pypath.Resolve(expanded)
	if err != nil {
		return "", false
	}
	return resolved, true
}

// Set key of a resolved home compared the way pathlib compares paths on this platform
func scopeKey(resolved string) string { return pypath.Normcase(resolved) }

// UpdateScopeHomes lists the resolved homes the running update owns, the invoking home, the root and its profiles
func UpdateScopeHomes(ctx context.Context) map[string]bool {
	homes := map[string]bool{}
	if resolved, ok := resolvedScopeHome(home.Dir(ctx)); ok {
		homes[scopeKey(resolved)] = true
	}
	if list, err := updaterecord.ProfileHomes(); err == nil {
		for _, ph := range list {
			if resolved, ok := resolvedScopeHome(ph.Home); ok {
				homes[scopeKey(resolved)] = true
			}
		}
	}
	return homes
}

// HomeInUpdateScope reports whether a home is one this update owns, the live scope when scope is nil
func HomeInUpdateScope(ctx context.Context, h string, scope map[string]bool) bool {
	if h == "" {
		return false
	}
	resolved, ok := resolvedScopeHome(h)
	if !ok {
		return false
	}
	if scope == nil {
		scope = UpdateScopeHomes(ctx)
	}
	return scope[scopeKey(resolved)]
}

// Home the spawn ledger records for a gateway pid on an exact live birth match, empty otherwise
func ledgerGatewayHome(pid int) string {
	for _, entry := range tools.LedgerEntries(tools.LedgerOptions{VerifiedOnly: true}) {
		if pyEq(entry["pid"], pid) && pyEq(entry["purpose"], "gateway") {
			h, _ := entry["hermes_home"].(string)
			return h
		}
	}
	return ""
}

// GatewayPidHome is the home a gateway pid runs on from its live environment, else its ledger witness
func GatewayPidHome(ctx context.Context, pid int) string {
	h, ok, err := homeForPIDHook(ctx, pid, false)
	if err != nil || !ok {
		h = ""
	}
	if h != "" {
		return h
	}
	return ledgerGatewayHome(pid)
}

// GatewayPidInUpdateScope reports whether a gateway pid runs on an owned home, known false when its home is unreadable
func GatewayPidInUpdateScope(ctx context.Context, pid int, scope map[string]bool) (inScope bool, known bool) {
	h := GatewayPidHome(ctx, pid)
	if h == "" {
		return false, false
	}
	return HomeInUpdateScope(ctx, h, scope), true
}

// PartitionGatewayPidsByScope splits pids into owned ones and foreign ones paired with their readable home
func PartitionGatewayPidsByScope(ctx context.Context, pids []int, scope map[string]bool) ([]int, []ForeignGatewayPid) {
	if scope == nil {
		scope = UpdateScopeHomes(ctx)
	}
	owned := []int{}
	foreign := []ForeignGatewayPid{}
	for _, pid := range pids {
		inScope, known := GatewayPidInUpdateScope(ctx, pid, scope)
		if known && inScope {
			owned = append(owned, pid)
			continue
		}
		h := ""
		if known {
			if found, ok, err := homeForPIDHook(ctx, pid, false); err == nil && ok {
				h = found
			}
		}
		foreign = append(foreign, ForeignGatewayPid{PID: pid, Home: h})
	}
	return owned, foreign
}
