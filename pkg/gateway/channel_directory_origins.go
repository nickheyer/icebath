package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Directory id of a session origin, chat and thread joined, empty without a chat id
func sessionEntryID(origin *pyjson.Object) string {
	chatID, _ := origin.Get("chat_id")
	if !pyrepr.Truthy(chatID) {
		return ""
	}
	if tid, _ := origin.Get("thread_id"); pyrepr.Truthy(tid) {
		return pyrepr.Str(chatID) + ":" + pyrepr.Str(tid)
	}
	return pyrepr.Str(chatID)
}

// Display name of a session origin, its topic or topic number appended for a thread
func sessionEntryName(origin *pyjson.Object) any {
	base, _ := origin.Get("chat_name")
	if !pyrepr.Truthy(base) {
		base, _ = origin.Get("user_name")
	}
	if !pyrepr.Truthy(base) {
		chatID, _ := origin.Get("chat_id")
		base = pyrepr.Str(chatID)
	}
	tid, _ := origin.Get("thread_id")
	if !pyrepr.Truthy(tid) {
		return base
	}
	topic, _ := origin.Get("chat_topic")
	if !pyrepr.Truthy(topic) {
		topic = "topic " + pyrepr.Str(tid)
	}
	return pyrepr.Str(base) + " / " + pyrepr.Str(topic)
}

// Known channels and contacts from session origins, state.db first and sessions.json as the fallback
func BuildFromSessions(ctx context.Context, platformName string) []*pyjson.Object {
	if out := buildFromSessionsDB(ctx, platformName); len(out) > 0 {
		return out
	}
	return buildFromSessionsJSON(ctx, platformName)
}

// Deduped entries of the yielded origins, keeping entries read before a failure
func entriesFromOrigins(platformName, source string, origins func(yield func(origin *pyjson.Object, chatType any)) error) []*pyjson.Object {
	var entries []*pyjson.Object
	seen := map[string]bool{}
	if err := origins(func(origin *pyjson.Object, chatType any) {
		id := sessionEntryID(origin)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		tid, _ := origin.Get("thread_id")
		entries = append(entries, pyjson.ObjectOf("id", id, "name", sessionEntryName(origin), "type", chatType, "thread_id", tid))
	}); err != nil {
		slog.Debug(fmt.Sprintf("Channel directory: %s for %s: %v", source, platformName, err))
	}
	return entries
}

// Entries from the gateway session rows of state.db
func buildFromSessionsDB(ctx context.Context, platformName string) []*pyjson.Object {
	return entriesFromOrigins(platformName, "state.db session read failed", func(yield func(*pyjson.Object, any)) error {
		db, err := state.Acquire(ctx, "")
		if err != nil {
			return err
		}
		rows, err := db.ListGatewaySessions(ctx, platformName, false)
		state.ReleaseOrClose(db)
		if err != nil {
			return err
		}
		for _, row := range rows {
			var origin *pyjson.Object
			if raw, _ := row.Get("origin_json"); pyrepr.Truthy(raw) {
				if text, ok := raw.(string); ok {
					if v, err := pyjson.Loads(text); err == nil {
						origin, _ = v.(*pyjson.Object)
					}
				}
			}
			if origin == nil || origin.Len() == 0 {
				chatID, _ := row.Get("chat_id")
				tid, _ := row.Get("thread_id")
				name, _ := row.Get("display_name")
				origin = pyjson.ObjectOf("chat_id", chatID, "thread_id", tid, "chat_name", name)
			}
			chatType, _ := row.Get("chat_type")
			if !pyrepr.Truthy(chatType) {
				chatType = "dm"
			}
			yield(origin, chatType)
		}
		return nil
	})
}

// Entries from the origin data of the legacy sessions.json index
func buildFromSessionsJSON(ctx context.Context, platformName string) []*pyjson.Object {
	path := filepath.Join(home.Dir(ctx), "sessions", "sessions.json")
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	return entriesFromOrigins(platformName, "failed to read sessions", func(yield func(*pyjson.Object, any)) error {
		raw, err := readChannelJSON(path)
		if err != nil {
			return err
		}
		sessions, ok := raw.(*pyjson.Object)
		if !ok || sessions == nil {
			return fmt.Errorf("'%s' object has no attribute 'items'", pyrepr.TypeName(raw))
		}
		for _, key := range append([]string(nil), sessions.Keys...) {
			v, _ := sessions.Get(key)
			session, ok := v.(*pyjson.Object)
			if strings.HasPrefix(key, "_") || !ok || session == nil {
				continue
			}
			var origin *pyjson.Object
			if o, _ := session.Get("origin"); pyrepr.Truthy(o) {
				obj, isObj := o.(*pyjson.Object)
				if !isObj {
					return fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(o))
				}
				origin = obj
			} else {
				origin = pyjson.NewObject()
			}
			if p, _ := origin.Get("platform"); p == any(platformName) {
				chatType, has := session.Get("chat_type")
				if !has {
					chatType = "dm"
				}
				yield(origin, chatType)
			}
		}
		return nil
	})
}

// Numeric value of a Python number, ok false for every other type
func directoryNumber(v any) (*big.Float, bool) {
	switch n := v.(type) {
	case bool:
		if n {
			return big.NewFloat(1), true
		}
		return big.NewFloat(0), true
	case int:
		return new(big.Float).SetInt64(int64(n)), true
	case int64:
		return new(big.Float).SetInt64(n), true
	case float64:
		return big.NewFloat(n), true
	case *big.Int:
		return new(big.Float).SetInt(n), true
	}
	return nil, false
}

// Python less than for strings and numbers, a TypeError for any other pairing
func pyLess(a, b any) (bool, error) {
	if sa, ok := a.(string); ok {
		if sb, ok := b.(string); ok {
			return sa < sb, nil
		}
	}
	if na, ok := directoryNumber(a); ok {
		if nb, ok := directoryNumber(b); ok {
			return na.Cmp(nb) < 0, nil
		}
	}
	return false, fmt.Errorf("'<' not supported between instances of '%s' and '%s'", pyrepr.TypeName(a), pyrepr.TypeName(b))
}

// Stable sort by Python ordering of the keys, the first comparison error returned
func pySortStable[T any](items []T, key func(T) any) error {
	var cmpErr error
	sort.SliceStable(items, func(i, j int) bool {
		less, err := pyLess(key(items[i]), key(items[j]))
		if err != nil && cmpErr == nil {
			cmpErr = err
		}
		return less
	})
	return cmpErr
}

// Channel dicts of a directory channel list, an error where Python iteration or dict access raises
func directoryChannelDicts(channels any) ([]*pyjson.Object, error) {
	var items []any
	switch c := channels.(type) {
	case []any:
		items = c
	case *pyjson.Object:
		for _, k := range c.Keys {
			items = append(items, k)
		}
	case string:
		for _, r := range c {
			items = append(items, string(r))
		}
	default:
		return nil, fmt.Errorf("'%s' object is not iterable", pyrepr.TypeName(channels))
	}
	out := make([]*pyjson.Object, 0, len(items))
	for _, it := range items {
		obj, ok := it.(*pyjson.Object)
		if !ok || obj == nil {
			return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(it))
		}
		out = append(out, obj)
	}
	return out, nil
}

// Channel directory as the human readable target list shown to the model, the on disk directory when nil
func FormatDirectoryForDisplay(ctx context.Context, platforms *pyjson.Object) (string, error) {
	var src any = platforms
	if platforms == nil {
		src, _ = loadChannelDirectory(ctx).Get("platforms")
		if src == nil {
			src = pyjson.NewObject()
		}
	}
	if !pyrepr.Truthy(src) {
		return "No messaging platforms connected or no channels discovered yet.", nil
	}
	pobj, ok := src.(*pyjson.Object)
	if !ok {
		return "", fmt.Errorf("'%s' object has no attribute 'items'", pyrepr.TypeName(src))
	}
	names := append([]string(nil), pobj.Keys...)
	sort.Strings(names)
	lines := []string{"Available messaging targets:\n"}
	for _, plat := range names {
		channels, _ := pobj.Get(plat)
		switch {
		case !pyrepr.Truthy(channels):
			lines = append(lines, pystr.Title(plat)+":",
				fmt.Sprintf("  (no channels discovered yet — send directly with %s:<chat_id>, or bare '%s' for the home channel)", plat, plat))
		case plat == "discord":
			chs, err := directoryChannelDicts(channels)
			if err != nil {
				return "", err
			}
			type guildGroup struct {
				guild any
				chs   []*pyjson.Object
			}
			var guilds []*guildGroup
			index := map[string]*guildGroup{}
			var dms []*pyjson.Object
			for _, ch := range chs {
				g, _ := ch.Get("guild")
				if !pyrepr.Truthy(g) {
					dms = append(dms, ch)
					continue
				}
				k := pyrepr.Repr(g)
				grp := index[k]
				if grp == nil {
					grp = &guildGroup{guild: g}
					index[k] = grp
					guilds = append(guilds, grp)
				}
				grp.chs = append(grp.chs, ch)
			}
			if err := pySortStable(guilds, func(g *guildGroup) any { return g.guild }); err != nil {
				return "", err
			}
			for _, grp := range guilds {
				for _, ch := range grp.chs {
					if _, has := ch.Get("name"); !has {
						return "", fmt.Errorf("'name'")
					}
				}
				if err := pySortStable(grp.chs, func(c *pyjson.Object) any { n, _ := c.Get("name"); return n }); err != nil {
					return "", err
				}
				lines = append(lines, fmt.Sprintf("Discord (%s):", pyrepr.Str(grp.guild)))
				for _, ch := range grp.chs {
					name, err := channelTargetName(plat, ch)
					if err != nil {
						return "", err
					}
					lines = append(lines, "  discord:"+name)
				}
			}
			if len(dms) > 0 {
				lines = append(lines, "Discord (DMs):")
				for _, ch := range dms {
					name, err := channelTargetName(plat, ch)
					if err != nil {
						return "", err
					}
					lines = append(lines, "  discord:"+name)
				}
			}
		default:
			chs, err := directoryChannelDicts(channels)
			if err != nil {
				return "", err
			}
			lines = append(lines, pystr.Title(plat)+":")
			for _, ch := range chs {
				name, err := channelTargetName(plat, ch)
				if err != nil {
					return "", err
				}
				lines = append(lines, "  "+plat+":"+name)
			}
		}
		lines = append(lines, "")
	}
	lines = append(lines, `Use these as the "target" parameter when sending.`, `Bare platform name (e.g. "telegram") sends to home channel.`)
	return strings.Join(lines, "\n"), nil
}
