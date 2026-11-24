package gateway

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Directory id of a session origin, chat and thread joined, empty without a chat id
func sessionEntryID(origin *jsonutil.Object) string {
	chatID, _ := origin.Get("chat_id")
	if !anyutil.IsTrue(chatID) {
		return ""
	}
	if tid, _ := origin.Get("thread_id"); anyutil.IsTrue(tid) {
		return anyutil.String(chatID) + ":" + anyutil.String(tid)
	}
	return anyutil.String(chatID)
}

// Display name of a session origin, its topic or topic number appended for a thread
func sessionEntryName(origin *jsonutil.Object) any {
	base, _ := origin.Get("chat_name")
	if !anyutil.IsTrue(base) {
		base, _ = origin.Get("user_name")
	}
	if !anyutil.IsTrue(base) {
		chatID, _ := origin.Get("chat_id")
		base = anyutil.String(chatID)
	}
	tid, _ := origin.Get("thread_id")
	if !anyutil.IsTrue(tid) {
		return base
	}
	topic, _ := origin.Get("chat_topic")
	if !anyutil.IsTrue(topic) {
		topic = "topic " + anyutil.String(tid)
	}
	return anyutil.String(base) + " / " + anyutil.String(topic)
}

// Known channels and contacts from session origins, state.db first and sessions.json as the fallback
func BuildFromSessions(ctx context.Context, platformName string) []*jsonutil.Object {
	if out := buildFromSessionsDB(ctx, platformName); len(out) > 0 {
		return out
	}
	return buildFromSessionsJSON(ctx, platformName)
}

// Deduped entries of the yielded origins, keeping entries read before a failure
func entriesFromOrigins(platformName, source string, origins func(yield func(origin *jsonutil.Object, chatType any)) error) []*jsonutil.Object {
	var entries []*jsonutil.Object
	seen := map[string]bool{}
	if err := origins(func(origin *jsonutil.Object, chatType any) {
		id := sessionEntryID(origin)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		tid, _ := origin.Get("thread_id")
		entries = append(entries, jsonutil.ObjectOf("id", id, "name", sessionEntryName(origin), "type", chatType, "thread_id", tid))
	}); err != nil {
		pkgLog().Debug(fmt.Sprintf("Channel directory: %s for %s: %v", source, platformName, err))
	}
	return entries
}

// Entries from the gateway session rows of state.db
func buildFromSessionsDB(ctx context.Context, platformName string) []*jsonutil.Object {
	return entriesFromOrigins(platformName, "state.db session read failed", func(yield func(*jsonutil.Object, any)) error {
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
			var origin *jsonutil.Object
			if raw, _ := row.Get("origin_json"); anyutil.IsTrue(raw) {
				if text, ok := raw.(string); ok {
					if v, err := jsonutil.Decode(text); err == nil {
						origin, _ = v.(*jsonutil.Object)
					}
				}
			}
			if origin == nil || origin.Len() == 0 {
				chatID, _ := row.Get("chat_id")
				tid, _ := row.Get("thread_id")
				name, _ := row.Get("display_name")
				origin = jsonutil.ObjectOf("chat_id", chatID, "thread_id", tid, "chat_name", name)
			}
			chatType, _ := row.Get("chat_type")
			if !anyutil.IsTrue(chatType) {
				chatType = "dm"
			}
			yield(origin, chatType)
		}
		return nil
	})
}

// Entries from the origin data of the legacy sessions.json index
func buildFromSessionsJSON(ctx context.Context, platformName string) []*jsonutil.Object {
	path := filepath.Join(home.Dir(ctx), "sessions", "sessions.json")
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	return entriesFromOrigins(platformName, "failed to read sessions", func(yield func(*jsonutil.Object, any)) error {
		raw, err := readChannelJSON(path)
		if err != nil {
			return err
		}
		sessions, ok := raw.(*jsonutil.Object)
		if !ok || sessions == nil {
			return fmt.Errorf("the sessions index is %s, expected a mapping", errchain.Kind(raw))
		}
		for _, key := range append([]string(nil), sessions.Keys()...) {
			v, _ := sessions.Get(key)
			session, ok := v.(*jsonutil.Object)
			if strings.HasPrefix(key, "_") || !ok || session == nil {
				continue
			}
			var origin *jsonutil.Object
			if o, _ := session.Get("origin"); anyutil.IsTrue(o) {
				obj, isObj := o.(*jsonutil.Object)
				if !isObj {
					return fmt.Errorf("a session origin is %s, expected a mapping", errchain.Kind(o))
				}
				origin = obj
			} else {
				origin = jsonutil.NewObject()
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
	return false, fmt.Errorf("cannot order %s against %s", errchain.Kind(a), errchain.Kind(b))
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
func directoryChannelDicts(channels any) ([]*jsonutil.Object, error) {
	var items []any
	switch c := channels.(type) {
	case []any:
		items = c
	case *jsonutil.Object:
		for _, k := range c.Keys() {
			items = append(items, k)
		}
	case string:
		for _, r := range c {
			items = append(items, string(r))
		}
	default:
		return nil, fmt.Errorf("the directory channels are %s, expected a list", errchain.Kind(channels))
	}
	out := make([]*jsonutil.Object, 0, len(items))
	for _, it := range items {
		obj, ok := it.(*jsonutil.Object)
		if !ok || obj == nil {
			return nil, fmt.Errorf("a directory channel is %s, expected a mapping", errchain.Kind(it))
		}
		out = append(out, obj)
	}
	return out, nil
}

// Channel directory as the human readable target list shown to the model, the on disk directory when nil
func FormatDirectoryForDisplay(ctx context.Context, platforms *jsonutil.Object) (string, error) {
	var src any = platforms
	if platforms == nil {
		src, _ = loadChannelDirectory(ctx).Get("platforms")
		if src == nil {
			src = jsonutil.NewObject()
		}
	}
	if !anyutil.IsTrue(src) {
		return "No messaging platforms connected or no channels discovered yet.", nil
	}
	pobj, ok := src.(*jsonutil.Object)
	if !ok {
		return "", fmt.Errorf("the channel directory is %s, expected a mapping", errchain.Kind(src))
	}
	names := append([]string(nil), pobj.Keys()...)
	sort.Strings(names)
	lines := []string{"Available messaging targets:\n"}
	for _, plat := range names {
		channels, _ := pobj.Get(plat)
		switch {
		case !anyutil.IsTrue(channels):
			lines = append(lines, textutil.Title(plat)+":",
				fmt.Sprintf("  (no channels discovered yet — send directly with %s:<chat_id>, or bare '%s' for the home channel)", plat, plat))
		case plat == "discord":
			chs, err := directoryChannelDicts(channels)
			if err != nil {
				return "", err
			}
			type guildGroup struct {
				guild any
				chs   []*jsonutil.Object
			}
			var guilds []*guildGroup
			index := map[string]*guildGroup{}
			var dms []*jsonutil.Object
			for _, ch := range chs {
				g, _ := ch.Get("guild")
				if !anyutil.IsTrue(g) {
					dms = append(dms, ch)
					continue
				}
				k := anyutil.Quote(g)
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
				if err := pySortStable(grp.chs, func(c *jsonutil.Object) any { n, _ := c.Get("name"); return n }); err != nil {
					return "", err
				}
				lines = append(lines, fmt.Sprintf("Discord (%s):", anyutil.String(grp.guild)))
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
			lines = append(lines, textutil.Title(plat)+":")
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
