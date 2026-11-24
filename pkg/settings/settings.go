// Package settings is the one typed registry of every user setting, its layered resolution and its single writer.
package settings

//go:generate go run -tags goolm ./internal/docgen -root ../..

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
)

// Logger named after this package
func pkgLog() *slog.Logger { return slog.Default().With("logger", "settings") }

// Value shape of a setting, which also picks its edit widget
type Kind string

const (
	KindBool     Kind = "bool"
	KindInt      Kind = "int"
	KindFloat    Kind = "float"
	KindDuration Kind = "duration"
	KindEnum     Kind = "enum"
	KindString   Kind = "string"
	KindPath     Kind = "path"
	KindSecret   Kind = "secret"
	KindList     Kind = "list"
	KindMap      Kind = "map"
	KindObject   Kind = "object"
	// Any YAML value, under a map that gives its entries no schema
	KindAny Kind = "any"
)

// Set of layers a setting may be written to
type Scope uint8

const (
	// Profile config.yaml
	Global Scope = 1 << iota
	// Project settings file, reserved and not yet writable
	Workspace
	// In memory for one session, gone when the session ends
	Session
)

// Words of a scope set, in resolution order
func (s Scope) Names() []string {
	var out []string
	for _, pair := range []struct {
		bit  Scope
		name string
	}{{Global, "global"}, {Workspace, "workspace"}, {Session, "session"}} {
		if s&pair.bit != 0 {
			out = append(out, pair.name)
		}
	}
	return out
}

// Word of a single scope
func (s Scope) String() string { return strings.Join(s.Names(), "|") }

// Scope a word names, zero for an unknown word
func ParseScope(word string) Scope {
	switch strings.ToLower(strings.TrimSpace(word)) {
	case "global", "profile":
		return Global
	case "workspace", "project":
		return Workspace
	case "session":
		return Session
	}
	return 0
}

// When a change takes effect
type Apply uint8

const (
	// On the next repaint or read
	Live Apply = iota
	// From the next turn of a running session
	NextTurn
	// Only in sessions started after the change
	NewSession
	// After the background service restarts
	Restart
)

// Word of an apply mode
func (a Apply) String() string {
	switch a {
	case NextTurn:
		return "next_turn"
	case NewSession:
		return "new_session"
	case Restart:
		return "restart"
	}
	return "live"
}

// Layer a resolved value came from, in rising precedence
type Source string

const (
	SourceDefault   Source = "default"
	SourceGlobal    Source = "global"
	SourceWorkspace Source = "workspace"
	SourceSession   Source = "session"
	SourceEnv       Source = "env"
	SourceManaged   Source = "managed"
)

// One allowed value of an enum with its row label and one line meaning
type Choice struct {
	Value   string `json:"value"`
	Label   string `json:"label"`
	Meaning string `json:"meaning,omitempty"`
}

// Description, placement and behaviour every setting shares
type Meta struct {
	// Dotted path in config.yaml, never reused for another meaning
	Key string
	// Row title, at most three words
	Label string
	// What the user sees change, at most 70 characters
	Short string
	// Optional detail, at most two sentences
	Help string
	// Settings screen group
	Group GroupID
	// Position inside the group
	Order int
	// Layers the setting may be written to
	Scopes Scope
	// When a change takes effect
	Apply Apply
	// Hidden behind show advanced
	Advanced bool
	// A chat platform may override it under display.platforms.<platform>, and its tier may give it another default
	Platforms bool
	// A chat platform never takes the global value, only its own override or its tier default
	PlatformOnly bool
	// Runtime state the app writes for itself, kept in the home's state file so no install can lock it
	State bool
	// Shortcut command that names this setting
	Slash string
	// One documented env override from the pre config allowlist
	Env string
	// The env override of an on or off setting says the opposite: on there turns the setting off
	EnvInvert bool
	// Extra check of a coerced value, a one line error
	Validate func(value any) error
	// Other keys written in the same change so their existing readers follow
	Companions func(value any) []config.KeyValue
	// Value computed from other settings, the setting then storing nothing of its own and writing only Companions
	Derive func(ctx context.Context) (any, Source)
}

// Metadata of the entry
func (m *Meta) Info() *Meta { return m }

// One registered setting
type Entry interface {
	Info() *Meta
	Kind() Kind
	// Default value, nil when the setting is unset by default
	DefaultValue() any
	// Canonical stored value of a written value, an error naming what is accepted
	Coerce(value any) (any, error)
	// Typed value of a stored value, false when it does not fit
	read(value any) (any, bool)
}

// No setting has this key or alias
var ErrUnknownKey = errors.New("unknown setting")

// The setting cannot be written to the requested scope
var ErrScope = errors.New("scope not allowed")

// An administrator pins the setting
var ErrLocked = errors.New("setting is locked")

// The request lacks the session a session write needs
var ErrNoSession = errors.New("session scope needs a session")

// Refusal of a value with the reason a user can act on
type ValueError struct {
	Key    string
	Reason string
}

func (e *ValueError) Error() string { return e.Key + ": " + e.Reason }

// Value refusal of a key
func invalid(key, format string, args ...any) error {
	return &ValueError{Key: key, Reason: fmt.Sprintf(format, args...)}
}

type sessionCtxKey struct{}

// Context whose reads and session writes belong to one session
func WithSession(ctx context.Context, sessionID string) context.Context {
	if sessionID == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionCtxKey{}, sessionID)
}

// Context whose reads leave out the session ctx belongs to, answering what the profile gives a session that chose nothing itself
func WithoutSession(ctx context.Context) context.Context {
	if SessionFrom(ctx) == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionCtxKey{}, "")
}

// Session a context belongs to, empty when none
func SessionFrom(ctx context.Context) string {
	sid, _ := ctx.Value(sessionCtxKey{}).(string)
	return sid
}
