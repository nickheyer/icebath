package gateway

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Subject a native ticket carries on this host, the owner's own OS identity
func HostOwnerSubject() string {
	return ownerWorkerSubject()
}

// Subject a connection acts as, every login collapsing onto the host owner and native grants keeping theirs
func PrincipalSubject(identity map[string]any) (string, error) {
	subject, err := AuthenticatedSubject(identity)
	if err != nil {
		return "", err
	}
	if !anyutil.IsTrue(identity["user_id"]) {
		return subject, nil
	}
	provider := ""
	if anyutil.IsTrue(identity["provider"]) {
		provider = strings.TrimSpace(anyutil.String(identity["provider"]))
	}
	if provider == "local" {
		login, ok := ParseAuthenticatedSubject(subject)
		if ok && loginProvider(login[0]) && login[0] != "local" {
			return HostOwnerSubject(), nil
		}
		return subject, nil
	}
	if loginProvider(provider) {
		return HostOwnerSubject(), nil
	}
	return subject, nil
}

var bundledLogins = map[string]bool{"basic": true, "self-hosted": true, "session-token": true, "server-internal": true}

// True for a bundled sign in
func loginProvider(provider string) bool {
	return bundledLogins[provider]
}

// True when a stored provider and user stamp names the host owner
func ownerLogin(provider, user string) bool {
	if provider == "local" {
		return strings.TrimSpace(user) == HostOwnerSubject()
	}
	return loginProvider(provider)
}

// True for a sign in provider and user label naming the owner
func ownerLoginLabel(value string) bool {
	provider, user, sep := strings.Cut(value, ":")
	return sep && strings.TrimSpace(user) != "" && ownerLogin(provider, user)
}

// True for an auth v1 subject a sign in stored that names the owner
func ownerLoginSubject(value any) bool {
	parts, ok := ParseAuthenticatedSubject(value)
	return ok && strings.TrimSpace(parts[2]) != "" && ownerLogin(parts[0], parts[2])
}

// True when a stored owner stamp belongs to subject, older sign in stamps reading as the host owner's
func SameOwner(stored, subject any) bool {
	if equalOwner(stored, subject) {
		return true
	}
	text, isText := stored.(string)
	if !isText || text == "" || !anyutil.IsTrue(subject) {
		return false
	}
	if label, ok := state.LoginLabel(subject); ok && text == label {
		return true
	}
	return subject == HostOwnerSubject() && (ownerLoginSubject(text) || ownerLoginLabel(text))
}

// True when two stored owner stamps name the same owner either way round
func OwnersAgree(first, second any) bool {
	return SameOwner(first, second) || SameOwner(second, first)
}

// Python equality of two owner stamps, strings and None compared by value
func equalOwner(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	as, aok := a.(string)
	bs, bok := b.(string)
	if aok && bok {
		return as == bs
	}
	return anyutil.Equal(a, b)
}
