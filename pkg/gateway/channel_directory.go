package gateway

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Explicit directory and alias file overrides tests set, empty reading the active home
var (
	channelDirectoryPath string
	channelAliasesPath   string
)

// Cached channel directory file of the active home unless overridden
func channelDirectoryFile(ctx context.Context) string {
	if channelDirectoryPath != "" {
		return channelDirectoryPath
	}
	return filepath.Join(home.Dir(ctx), "channel_directory.json")
}

// User maintained friendly name overlay file of the active home unless overridden
func channelAliasesFile(ctx context.Context) string {
	if channelAliasesPath != "" {
		return channelAliasesPath
	}
	return filepath.Join(home.Dir(ctx), "channel_aliases.json")
}

// JSON of a file decoded as utf-8-sig so a leading BOM is accepted
func readChannelJSON(path string) (any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("UnicodeDecodeError: 'utf-8' codec can't decode %s", path)
	}
	return pyjson.Loads(string(raw))
}

// JSON object of a file, empty when missing, unreadable or not an object
func loadChannelJSONDict(path string) *pyjson.Object {
	if _, err := os.Stat(path); err != nil {
		return pyjson.NewObject()
	}
	data, err := readChannelJSON(path)
	if obj, ok := data.(*pyjson.Object); err == nil && ok && obj != nil {
		return obj
	}
	return pyjson.NewObject()
}

// Overlays alias names onto entries by chat id in place, adding an entry for an alias not yet discovered
func applyChannelAliases(ctx context.Context, platforms *pyjson.Object) {
	aliases := loadChannelJSONDict(channelAliasesFile(ctx))
	for _, platName := range aliases.Keys {
		idMap, ok := aliases.Vals[platName].(*pyjson.Object)
		if !ok {
			continue
		}
		if _, has := platforms.Get(platName); !has {
			platforms.Set(platName, []any{})
		}
		raw, _ := platforms.Get(platName)
		entries, ok := raw.([]any)
		if !ok {
			continue
		}
		for _, chatID := range idMap.Keys {
			friendly, ok := idMap.Vals[chatID].(string)
			if !ok || pystr.Strip(friendly) == "" {
				continue
			}
			friendly = pystr.Strip(friendly)
			matched := false
			for _, e := range entries {
				if entry, isObj := e.(*pyjson.Object); isObj {
					if id, _ := entry.Get("id"); id == chatID {
						entry.Set("name", friendly)
						matched = true
					}
				}
			}
			if !matched {
				kind := "dm"
				if strings.HasSuffix(chatID, "@g.us") {
					kind = "group"
				}
				entries = append(entries, pyjson.ObjectOf("id", chatID, "name", friendly, "thread_id", nil, "type", kind))
			}
		}
		platforms.Set(platName, entries)
	}
}

// Whether the alias overlay holds a platform mapping that would write into the directory
func aliasesNamePlatforms(ctx context.Context) bool {
	aliases := loadChannelJSONDict(channelAliasesFile(ctx))
	for _, key := range aliases.Keys {
		if _, ok := aliases.Vals[key].(*pyjson.Object); ok {
			return true
		}
	}
	return false
}

// Leading hashes trimmed, whitespace stripped and lowercased
func normalizeChannelQuery(value string) string {
	return pystr.Lower(pystr.Strip(strings.TrimLeft(value, "#")))
}

// Human facing target label of a directory entry
func channelTargetName(platformName string, channel *pyjson.Object) (string, error) {
	rawName, has := channel.Get("name")
	if !has {
		return "", fmt.Errorf("KeyError: 'name'")
	}
	name := pyrepr.Str(rawName)
	guild, _ := channel.Get("guild")
	if platformName == "discord" {
		if pyrepr.Truthy(guild) {
			return "#" + name, nil
		}
		return name, nil
	}
	if kind, _ := channel.Get("type"); pyrepr.Truthy(kind) {
		return name + " (" + pyrepr.Str(kind) + ")", nil
	}
	return name, nil
}

// Cached directory with aliases applied on read, an empty directory when the file is missing or unusable
func loadChannelDirectory(ctx context.Context) *pyjson.Object {
	path := channelDirectoryFile(ctx)
	if _, err := os.Stat(path); err == nil {
		raw, err := readChannelJSON(path)
		if data, ok := raw.(*pyjson.Object); err == nil && ok && data != nil {
			if _, has := data.Get("platforms"); !has {
				data.Set("platforms", pyjson.NewObject())
			}
			platforms, _ := data.Get("platforms")
			if obj, isObj := platforms.(*pyjson.Object); isObj && obj != nil {
				applyChannelAliases(ctx, obj)
				return data
			}
			if !aliasesNamePlatforms(ctx) {
				return data
			}
		}
	}
	base := pyjson.ObjectOf("updated_at", nil, "platforms", pyjson.NewObject())
	platforms, _ := base.Get("platforms")
	applyChannelAliases(ctx, platforms.(*pyjson.Object))
	return base
}

// Entries the directory lists for a platform, an error where Python would raise reading them
func directoryChannels(ctx context.Context, platformName string, iterateFalsy bool) ([]*pyjson.Object, error) {
	rawPlatforms, has := loadChannelDirectory(ctx).Get("platforms")
	if !has {
		return nil, nil
	}
	platforms, ok := rawPlatforms.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(rawPlatforms))
	}
	raw, has := platforms.Get(platformName)
	if !has {
		return nil, nil
	}
	if !pyrepr.Truthy(raw) {
		switch raw.(type) {
		case []any, string, *pyjson.Object:
			return nil, nil
		}
		if iterateFalsy {
			return nil, fmt.Errorf("TypeError: '%s' object is not iterable", pyrepr.TypeName(raw))
		}
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("TypeError: unusable channel list of type '%s'", pyrepr.TypeName(raw))
	}
	out := make([]*pyjson.Object, len(list))
	for i, item := range list {
		entry, ok := item.(*pyjson.Object)
		if !ok {
			return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(item))
		}
		out[i] = entry
	}
	return out, nil
}

// Channel type of a chat id in the directory, empty when unknown
func lookupChannelType(ctx context.Context, platformName, chatID string) (any, error) {
	channels, err := directoryChannels(ctx, platformName, true)
	if err != nil {
		return nil, err
	}
	for _, ch := range channels {
		if id, _ := ch.Get("id"); id == chatID {
			kind, _ := ch.Get("type")
			return kind, nil
		}
	}
	return nil, nil
}

// Name field of an entry normalized, the KeyError or AttributeError Python raises when unusable
func channelEntryQueryName(ch *pyjson.Object) (string, error) {
	raw, has := ch.Get("name")
	if !has {
		return "", fmt.Errorf("KeyError: 'name'")
	}
	name, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("AttributeError: '%s' object has no attribute 'lstrip'", pyrepr.TypeName(raw))
	}
	return normalizeChannelQuery(name), nil
}

// Chat id a friendly channel name resolves to by exact id, name, guild qualified name or unique prefix
func resolveChannelName(ctx context.Context, platformName, name string) (string, error) {
	channels, err := directoryChannels(ctx, platformName, false)
	if err != nil || len(channels) == 0 {
		return "", err
	}
	raw := pystr.Strip(name)
	for _, ch := range channels {
		if id, _ := ch.Get("id"); id == raw {
			return raw, nil
		}
	}
	query := normalizeChannelQuery(name)
	for _, ch := range channels {
		entryName, err := channelEntryQueryName(ch)
		if err != nil {
			return "", err
		}
		label, err := channelTargetName(platformName, ch)
		if err != nil {
			return "", err
		}
		if query == entryName || query == normalizeChannelQuery(label) {
			return channelEntryID(ch)
		}
	}
	if i := strings.LastIndex(query, "/"); i >= 0 {
		guildPart, chPart := query[:i], query[i+1:]
		for _, ch := range channels {
			rawGuild, has := ch.Get("guild")
			if !has {
				rawGuild = ""
			}
			guild, ok := rawGuild.(string)
			if !ok {
				return "", fmt.Errorf("AttributeError: '%s' object has no attribute 'strip'", pyrepr.TypeName(rawGuild))
			}
			if pystr.Lower(pystr.Strip(guild)) != guildPart {
				continue
			}
			entryName, err := channelEntryQueryName(ch)
			if err != nil {
				return "", err
			}
			if entryName == chPart {
				return channelEntryID(ch)
			}
		}
	}
	var matches []*pyjson.Object
	for _, ch := range channels {
		entryName, err := channelEntryQueryName(ch)
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(entryName, query) {
			matches = append(matches, ch)
		}
	}
	if len(matches) == 1 {
		return channelEntryID(matches[0])
	}
	return "", nil
}

// Id of a matched entry as the str a send target reads, the KeyError Python raises when absent
func channelEntryID(ch *pyjson.Object) (string, error) {
	id, has := ch.Get("id")
	if !has {
		return "", fmt.Errorf("KeyError: 'id'")
	}
	return pyrepr.Str(id), nil
}
