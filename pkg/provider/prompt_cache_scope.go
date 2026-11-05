package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/state"
)

const (
	declaredScopePrefix = "gwk_"
	// ForkScopeSeparator joins a scope and a fork tag, doubled so single colon gateway keys never read as forks
	ForkScopeSeparator = "::"
)

var grokAggregatorModelPrefixes = []string{"x-ai/grok-", "xai/grok-"}

type cacheScopeLineageReader interface {
	GetCompressionLineage(sessionID string) ([]string, error)
}

type cacheScopeSessionReader interface {
	GetSession(id string) (*state.Session, error)
}

type cacheScopeForkReader interface {
	IsExplicitForkChild(sessionID string) (bool, error)
}

type cacheScopeIdentityReader interface {
	DeclaredScopeIdentity(sessionID string) (isFork bool, source string, err error)
}

type cacheScopeBoundaryReader interface {
	LatestConversationBoundary(sessionKey, source string) (generation int64, ok bool, err error)
}

type cacheScopeMemoKey struct {
	sessionID string
	hasDB     bool
}

// CacheScopeAgent carries the agent state the prompt cache scope resolution reads and memoizes
type CacheScopeAgent struct {
	SessionID           string
	SessionDB           any
	GatewaySessionKey   string
	PersistDisabled     bool
	InheritedCacheScope string
	Platform            string
	PromptCacheForkTag  string
	CompressionCount    func() int
	Provider            string
	Model               string
	BaseURL             string

	memoMu    sync.Mutex
	memoSet   bool
	memoKey   cacheScopeMemoKey
	memoScope string
}

// Compression lineage root of the session, empty when the store is absent, failing or returns no root
func cacheScopeLineageRoot(sessionID string, db any) string {
	reader, ok := db.(cacheScopeLineageReader)
	if db == nil || !ok {
		return ""
	}
	lineage, err := reader.GetCompressionLineage(sessionID)
	if err != nil || len(lineage) == 0 {
		return ""
	}
	return lineage[0]
}

// Session source the conversation is recorded under, the row value first and the persistence resolver second
func cacheScopeAgentSource(a *CacheScopeAgent, sessionID string, db any, rowSource *string) string {
	if rowSource == nil && sessionID != "" && db != nil {
		read := ""
		if reader, ok := db.(cacheScopeSessionReader); ok {
			if row, err := reader.GetSession(sessionID); err == nil && row != nil {
				read = pyStripSpace(row.Source)
			}
		}
		rowSource = &read
	}
	if rowSource != nil && *rowSource != "" {
		return *rowSource
	}
	return pyStripSpace(sessionSourceFor(a.Platform))
}

// Durable reset generation of the declared conversation key, empty when the store records none
func cacheScopeConversationGeneration(sessionKey, source string, db any) (string, error) {
	reader, ok := db.(cacheScopeBoundaryReader)
	if !ok {
		return "", nil
	}
	generation, present, err := reader.LatestConversationBoundary(sessionKey, source)
	if err != nil || !present {
		return "", err
	}
	return strconv.FormatInt(generation, 10), nil
}

// DeclaredConversationScope hashes the host declared conversation key into a gwk scope, empty when none applies
func DeclaredConversationScope(a *CacheScopeAgent) string {
	if strings.HasPrefix(a.InheritedCacheScope, declaredScopePrefix) {
		return a.InheritedCacheScope
	}
	key := pyStripSpace(a.GatewaySessionKey)
	if key == "" || a.PersistDisabled {
		return ""
	}
	sid := a.SessionID
	db := a.SessionDB
	var rowSource *string
	if sid != "" && db != nil {
		var isFork bool
		if identity, ok := db.(cacheScopeIdentityReader); ok {
			fork, src, err := identity.DeclaredScopeIdentity(sid)
			if err != nil {
				return ""
			}
			isFork, rowSource = fork, &src
		} else if forks, ok := db.(cacheScopeForkReader); ok {
			fork, err := forks.IsExplicitForkChild(sid)
			if err != nil {
				return ""
			}
			isFork = fork
		} else {
			return ""
		}
		if isFork {
			return ""
		}
	}
	source := cacheScopeAgentSource(a, sid, db, rowSource)
	generation := ""
	if db != nil {
		g, err := cacheScopeConversationGeneration(key, source, db)
		if err != nil {
			return ""
		}
		generation = g
	}
	sum := sha256.Sum256(pyUTF8Replace(source + "|" + key + "|" + generation))
	return declaredScopePrefix + hex.EncodeToString(sum[:])[:24]
}

// ResolvePromptCacheScope returns the rotation stable cache scope, memoized per session id and store presence
func ResolvePromptCacheScope(a *CacheScopeAgent) string {
	if a.InheritedCacheScope != "" {
		return applyCacheScopeForkTag(a, a.InheritedCacheScope)
	}
	sid := a.SessionID
	if sid == "" {
		return ""
	}
	db := a.SessionDB
	key := cacheScopeMemoKey{sessionID: sid, hasDB: db != nil}
	a.memoMu.Lock()
	memoHit, memoScope := a.memoSet && a.memoKey == key, a.memoScope
	a.memoMu.Unlock()
	if memoHit {
		return applyCacheScopeForkTag(a, memoScope)
	}
	root := DeclaredConversationScope(a)
	if root == "" {
		root = cacheScopeLineageRoot(sid, db)
	}
	scope := root
	if scope == "" {
		scope = sid
	}
	if root != "" || db == nil || a.PersistDisabled {
		a.memoMu.Lock()
		a.memoSet, a.memoKey, a.memoScope = true, key, scope
		a.memoMu.Unlock()
	}
	return applyCacheScopeForkTag(a, scope)
}

// IsSlotKeyedCacheRoute reports whether the cache key pins one server side slot per conversation as xAI does
func IsSlotKeyedCacheRoute(providerName, model, baseURL string) bool {
	p := strings.ToLower(pyStripSpace(providerName))
	if p == "xai" || p == "xai-oauth" {
		return true
	}
	if baseURLHostname(baseURL) == "api.x.ai" {
		return true
	}
	m := strings.ToLower(pyStripSpace(model))
	for _, prefix := range grokAggregatorModelPrefixes {
		if strings.HasPrefix(m, prefix) {
			return true
		}
	}
	return false
}

// IsForkCacheScope reports whether the scope carries a fork tag
func IsForkCacheScope(scope string) bool {
	return strings.Contains(scope, ForkScopeSeparator)
}

// Appends the fork tag once the fork compacted on a slot keyed route, reading the route on every call
func applyCacheScopeForkTag(a *CacheScopeAgent, scope string) string {
	tag := a.PromptCacheForkTag
	if scope == "" || tag == "" {
		return scope
	}
	count := 0
	if a.CompressionCount != nil {
		count = a.CompressionCount()
	}
	if count < 1 || !IsSlotKeyedCacheRoute(a.Provider, a.Model, a.BaseURL) {
		return scope
	}
	return scope + ForkScopeSeparator + tag
}

// DeclaredConversationScopeSafe resolves the declared scope and turns any panic into an empty scope
func DeclaredConversationScopeSafe(a *CacheScopeAgent) (scope string) {
	defer func() {
		if recover() != nil {
			scope = ""
		}
	}()
	return DeclaredConversationScope(a)
}

// ResolvePromptCacheScopeSafe resolves the scope and turns any panic into an empty scope meaning the physical id
func ResolvePromptCacheScopeSafe(a *CacheScopeAgent) (scope string) {
	defer func() {
		if recover() != nil {
			scope = ""
		}
	}()
	return ResolvePromptCacheScope(a)
}

// Strips the characters Python str.strip treats as whitespace
func pyStripSpace(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		return (r >= 0x1c && r <= 0x1f) || unicode.IsSpace(r)
	})
}

// UTF-8 bytes with each surrogate or invalid byte replaced by a question mark as Python errors replace encodes
func pyUTF8Replace(s string) []byte {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		if i+2 < len(s) && s[i] == 0xED && s[i+1] >= 0xA0 && s[i+1] <= 0xBF && s[i+2] >= 0x80 && s[i+2] <= 0xBF {
			out = append(out, '?')
			i += 3
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			out = append(out, '?')
		} else {
			out = append(out, s[i:i+size]...)
		}
		i += size
	}
	return out
}
