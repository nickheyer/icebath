package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
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
		return nil, fmt.Errorf("%s is not valid UTF-8", path)
	}
	return jsonutil.Decode(string(raw))
}

// JSON object of a file, empty when missing, unreadable or not an object
func loadChannelJSONDict(path string) *jsonutil.Object {
	if _, err := os.Stat(path); err != nil {
		return jsonutil.NewObject()
	}
	data, err := readChannelJSON(path)
	if obj, ok := data.(*jsonutil.Object); err == nil && ok && obj != nil {
		return obj
	}
	return jsonutil.NewObject()
}

// Overlays alias names onto entries by chat id in place, adding an entry for an alias not yet discovered
func applyChannelAliases(ctx context.Context, platforms *jsonutil.Object) {
	aliases := loadChannelJSONDict(channelAliasesFile(ctx))
	for _, platName := range aliases.Keys() {
		idMap, ok := aliases.Value(platName).(*jsonutil.Object)
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
		for _, chatID := range idMap.Keys() {
			friendly, ok := idMap.Value(chatID).(string)
			if !ok || strings.TrimSpace(friendly) == "" {
				continue
			}
			friendly = strings.TrimSpace(friendly)
			matched := false
			for _, e := range entries {
				if entry, isObj := e.(*jsonutil.Object); isObj {
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
				entries = append(entries, jsonutil.ObjectOf("id", chatID, "name", friendly, "thread_id", nil, "type", kind))
			}
		}
		platforms.Set(platName, entries)
	}
}

// Whether the alias overlay holds a platform mapping that would write into the directory
func aliasesNamePlatforms(ctx context.Context) bool {
	aliases := loadChannelJSONDict(channelAliasesFile(ctx))
	for _, key := range aliases.Keys() {
		if _, ok := aliases.Value(key).(*jsonutil.Object); ok {
			return true
		}
	}
	return false
}

// Leading hashes trimmed, whitespace stripped and lowercased
func normalizeChannelQuery(value string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimLeft(value, "#")))
}

// Human facing target label of a directory entry
func channelTargetName(platformName string, channel *jsonutil.Object) (string, error) {
	rawName, has := channel.Get("name")
	if !has {
		return "", errors.New("a channel directory entry has no name")
	}
	name := anyutil.String(rawName)
	guild, _ := channel.Get("guild")
	if platformName == "discord" {
		if anyutil.IsTrue(guild) {
			return "#" + name, nil
		}
		return name, nil
	}
	if kind, _ := channel.Get("type"); anyutil.IsTrue(kind) {
		return name + " (" + anyutil.String(kind) + ")", nil
	}
	return name, nil
}

// Cached directory with aliases applied on read, an empty directory when the file is missing or unusable
func loadChannelDirectory(ctx context.Context) *jsonutil.Object {
	path := channelDirectoryFile(ctx)
	if _, err := os.Stat(path); err == nil {
		raw, err := readChannelJSON(path)
		if data, ok := raw.(*jsonutil.Object); err == nil && ok && data != nil {
			if _, has := data.Get("platforms"); !has {
				data.Set("platforms", jsonutil.NewObject())
			}
			platforms, _ := data.Get("platforms")
			if obj, isObj := platforms.(*jsonutil.Object); isObj && obj != nil {
				applyChannelAliases(ctx, obj)
				return data
			}
			if !aliasesNamePlatforms(ctx) {
				return data
			}
		}
	}
	base := jsonutil.ObjectOf("updated_at", nil, "platforms", jsonutil.NewObject())
	platforms, _ := base.Get("platforms")
	applyChannelAliases(ctx, platforms.(*jsonutil.Object))
	return base
}

// Entries the directory lists for a platform, an error where Python would raise reading them
func directoryChannels(ctx context.Context, platformName string, iterateFalsy bool) ([]*jsonutil.Object, error) {
	rawPlatforms, has := loadChannelDirectory(ctx).Get("platforms")
	if !has {
		return nil, nil
	}
	platforms, ok := rawPlatforms.(*jsonutil.Object)
	if !ok {
		return nil, fmt.Errorf("the channel directory platforms are %s, expected a mapping", errchain.Kind(rawPlatforms))
	}
	raw, has := platforms.Get(platformName)
	if !has {
		return nil, nil
	}
	if !anyutil.IsTrue(raw) {
		switch raw.(type) {
		case []any, string, *jsonutil.Object:
			return nil, nil
		}
		if iterateFalsy {
			return nil, fmt.Errorf("the %s channel directory list is %s, expected a list", platformName, errchain.Kind(raw))
		}
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("the %s channel directory list is %s, expected a list", platformName, errchain.Kind(raw))
	}
	out := make([]*jsonutil.Object, len(list))
	for i, item := range list {
		entry, ok := item.(*jsonutil.Object)
		if !ok {
			return nil, fmt.Errorf("a %s channel directory entry is %s, expected a mapping", platformName, errchain.Kind(item))
		}
		out[i] = entry
	}
	return out, nil
}

// Channel type of a chat id in the directory, nil when unknown
func LookupChannelType(ctx context.Context, platformName, chatID string) (any, error) {
	return lookupChannelType(ctx, platformName, chatID)
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
func channelEntryQueryName(ch *jsonutil.Object) (string, error) {
	raw, has := ch.Get("name")
	if !has {
		return "", errors.New("a channel directory entry has no name")
	}
	name, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("a channel directory entry name is %s, expected text", errchain.Kind(raw))
	}
	return normalizeChannelQuery(name), nil
}

// Chat id a friendly channel name resolves to by exact id, name, guild qualified name or unique prefix
func resolveChannelName(ctx context.Context, platformName, name string) (string, error) {
	channels, err := directoryChannels(ctx, platformName, false)
	if err != nil || len(channels) == 0 {
		return "", err
	}
	raw := strings.TrimSpace(name)
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
				return "", fmt.Errorf("a channel directory guild is %s, expected text", errchain.Kind(rawGuild))
			}
			if strings.ToLower(strings.TrimSpace(guild)) != guildPart {
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
	var matches []*jsonutil.Object
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
func channelEntryID(ch *jsonutil.Object) (string, error) {
	id, has := ch.Get("id")
	if !has {
		return "", errors.New("a channel directory entry has no id")
	}
	return anyutil.String(id), nil
}
