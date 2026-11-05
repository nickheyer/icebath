package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
)

var whatsappIdentityPlatforms = map[string]bool{"whatsapp": true, "whatsapp_cloud": true}

var whatsappNonUserSuffixes = []string{"@g.us", "@broadcast", "@newsletter"}

// True for group, broadcast and newsletter JIDs, which are chats rather than senders
func isWhatsAppNonUserChat(chatID *string) bool {
	if chatID == nil || *chatID == "" {
		return false
	}
	lowered := pystr.Lower(pystr.Strip(*chatID))
	for _, s := range whatsappNonUserSuffixes {
		if strings.HasSuffix(lowered, s) {
			return true
		}
	}
	return false
}

// True when two WhatsApp user chat ids name the same person across number, JID and LID forms
func whatsappUserChatIDsMatch(ctx context.Context, platform string, left, right *string) (bool, error) {
	if !whatsappIdentityPlatforms[pystr.Lower(pystr.Strip(platform))] || left == nil || *left == "" || right == nil || *right == "" ||
		isWhatsAppNonUserChat(left) || isWhatsAppNonUserChat(right) {
		return false, nil
	}
	leftAliases, err := expandWhatsAppAliases(ctx, *left)
	if err != nil || len(leftAliases) == 0 {
		return false, err
	}
	rightAliases, err := expandWhatsAppAliases(ctx, *right)
	if err != nil {
		return false, err
	}
	for a := range rightAliases {
		if leftAliases[a] {
			return true, nil
		}
	}
	return false, nil
}

// An explicit route matched a profile this gateway does not serve
type ProfileRouteRejected struct {
	Args []any
}

// The rejected profile when the error carries one
func (e *ProfileRouteRejected) Profile() any {
	if len(e.Args) > 1 {
		return e.Args[1]
	}
	return nil
}

func (e *ProfileRouteRejected) Error() string {
	if len(e.Args) == 0 {
		return ""
	}
	return pyrepr.Str(e.Args[0])
}

// One routing rule mapping a platform scope to a profile
type ProfileRoute struct {
	Name       any
	Platform   any
	Profile    string
	GuildID    *string
	ChatID     *string
	ThreadID   *string
	Enabled    any
	BotProfile *string
	UserID     *string
}

func nonEmpty(p *string) bool { return p != nil && *p != "" }

// Match weight, a higher value is more specific
func (r *ProfileRoute) Specificity() int {
	n := 0
	for i, p := range []*string{r.GuildID, r.ChatID, r.ThreadID, r.UserID} {
		if nonEmpty(p) {
			n += 2 << i
		}
	}
	return n
}

// Inbound discriminators a route is matched against, nil for an absent value
type RouteQuery struct {
	Platform       string
	GuildID        *string
	ChatID         *string
	ThreadID       *string
	ParentChatID   *string
	AdapterProfile *string
	UserID         *string
}

func strPtrEq(a, b *string) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }

// True when every discriminator the route declares holds
func (r *ProfileRoute) Matches(ctx context.Context, q RouteQuery) (bool, error) {
	if !pyrepr.Truthy(r.Enabled) || !pyrepr.Equal(r.Platform, q.Platform) {
		return false, nil
	}
	if !strPtrEq(botProfileKey(r.BotProfile), botProfileKey(q.AdapterProfile)) {
		return false, nil
	}
	if r.UserID != nil && (pystr.Strip(*r.UserID) == "" || !strPtrEq(r.UserID, q.UserID)) {
		return false, nil
	}
	if nonEmpty(r.ThreadID) && !strPtrEq(r.ThreadID, q.ThreadID) {
		return false, nil
	}
	if nonEmpty(r.ChatID) && !strPtrEq(r.ChatID, q.ChatID) && !strPtrEq(r.ChatID, q.ParentChatID) {
		viaAlias := false
		for _, candidate := range []*string{q.ChatID, q.ParentChatID} {
			same, err := whatsappUserChatIDsMatch(ctx, q.Platform, r.ChatID, candidate)
			if err != nil {
				return false, err
			}
			if viaAlias = same; same {
				break
			}
		}
		if !viaAlias {
			return false, nil
		}
	}
	return !(nonEmpty(r.GuildID) && !strPtrEq(r.GuildID, q.GuildID)), nil
}

// Mapping of every field, user_id only when set
func (r *ProfileRoute) ToDict() map[string]any {
	opt := func(p *string) any {
		if p == nil {
			return nil
		}
		return *p
	}
	out := map[string]any{"name": r.Name, "platform": r.Platform, "profile": r.Profile, "guild_id": opt(r.GuildID),
		"chat_id": opt(r.ChatID), "thread_id": opt(r.ThreadID), "enabled": r.Enabled, "bot_profile": opt(r.BotProfile)}
	if r.UserID != nil {
		out["user_id"] = *r.UserID
	}
	return out
}

// Nil for the default profile, else the profile name
func botProfileKey(name *string) *string {
	if name == nil {
		return nil
	}
	s := pystr.Strip(*name)
	if s == "" || s == "default" {
		return nil
	}
	return &s
}

// Route discriminator as a string, ints stringified and other types warned about
func coerceRouteID(value any) *string {
	if value == nil {
		return nil
	}
	if s, ok := value.(string); ok {
		return &s
	}
	if _, ok := pyrepr.Int(value); ok {
		s := pyrepr.Str(value)
		return &s
	}
	slog.Warn(fmt.Sprintf("Profile route discriminator %s (type %s) can never match an inbound id — quote it in config.yaml (e.g. chat_id: \"%s\").",
		pyrepr.Repr(value), pyrepr.TypeName(value), pyrepr.Str(value)))
	s := pyrepr.Str(value)
	return &s
}

// Profile routes of config.yaml sorted most specific first, TypeError for a non iterable value
func ParseProfileRoutes(ctx context.Context, raw any) ([]*ProfileRoute, error) {
	if !pyrepr.Truthy(raw) {
		return []*ProfileRoute{}, nil
	}
	var items []any
	switch v := raw.(type) {
	case []any:
		items = v
	case string, map[string]any:
	default:
		return nil, &PyError{"TypeError", "'" + pyrepr.TypeName(raw) + "' object is not iterable"}
	}
	routes := []*ProfileRoute{}
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, ok := entry["name"]
		if !ok {
			name = ""
		}
		platform, ok := entry["platform"]
		if !ok {
			platform = ""
		}
		profile, ok := entry["profile"]
		if !ok {
			profile = ""
		}
		if !pyrepr.Truthy(platform) || !pyrepr.Truthy(profile) {
			slog.Warn(fmt.Sprintf("Skipping profile route %s: missing platform or profile", pyrepr.Str(name)))
			continue
		}
		normalized, err := profiles.NormalizeProfileNameValue(profile)
		if err == nil {
			profile = normalized
			err = profiles.ValidateProfileName(normalized)
		}
		if err != nil {
			slog.Warn(fmt.Sprintf("Skipping profile route %s: invalid profile name %s", pyrepr.Str(name), pyrepr.Repr(profile)))
			continue
		}
		userID, hasUser := entry["user_id"]
		if s, isStr := userID.(string); hasUser && (userID == nil || isStr && pystr.Strip(s) == "") {
			slog.Warn(fmt.Sprintf("Skipping profile route %s: user_id cannot be null or empty", pyrepr.Str(name)))
			continue
		}
		enabled, ok := entry["enabled"]
		if !ok {
			enabled = true
		}
		var bot *string
		if b := entry["bot_profile"]; pyrepr.Truthy(b) {
			s, isStr := b.(string)
			if !isStr {
				return nil, &PyError{"AttributeError", "'" + pyrepr.TypeName(b) + "' object has no attribute 'strip'"}
			}
			bot = botProfileKey(&s)
		}
		routes = append(routes, &ProfileRoute{
			Name: name, Platform: platform, Profile: normalized,
			GuildID: coerceRouteID(entry["guild_id"]), ChatID: coerceRouteID(entry["chat_id"]),
			ThreadID: coerceRouteID(entry["thread_id"]), UserID: coerceRouteID(userID),
			Enabled: enabled, BotProfile: bot,
		})
	}
	sort.SliceStable(routes, func(i, j int) bool { return routes[i].Specificity() > routes[j].Specificity() })
	slog.Debug(fmt.Sprintf("Loaded %d profile routes (most-specific-first)", len(routes)))
	return routes, nil
}

// First, most specific, route matching the query
func MatchProfileRoute(ctx context.Context, routes []*ProfileRoute, q RouteQuery) (*ProfileRoute, error) {
	for _, r := range routes {
		ok, err := r.Matches(ctx, q)
		if err != nil {
			return nil, err
		}
		if ok {
			return r, nil
		}
	}
	return nil, nil
}
