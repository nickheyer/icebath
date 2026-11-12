package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"math/big"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-telegram/bot"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Creates a forum topic in a private chat, nil when the bot is absent or the topic cannot be created
func (a *TelegramAdapter) CreateDMTopic(ctx context.Context, chatID int64, name string, iconColor *int, iconCustomEmojiID *string) (*int64, error) {
	b := a.currentBot()
	if b == nil {
		return nil, nil
	}
	params := &bot.CreateForumTopicParams{ChatID: chatID, Name: name}
	if iconColor != nil {
		params.IconColor = *iconColor
	}
	if iconCustomEmojiID != nil && *iconCustomEmojiID != "" {
		params.IconCustomEmojiID = *iconCustomEmojiID
	}
	topic, err := b.CreateForumTopic(ctx, params)
	if err != nil {
		err = ptbError(err)
		low := errLower(err)
		switch {
		case strings.Contains(low, "topic_name_duplicate") || strings.Contains(low, "already"):
			slog.Info(fmt.Sprintf("[%s] DM topic '%s' already exists in chat %d (will be mapped from incoming messages)", a.Name(), name, chatID))
		case strings.Contains(low, "not a forum") || strings.Contains(low, "forums_disabled"):
			slog.Warn(fmt.Sprintf("[%s] Cannot create DM topic '%s' in chat %d: Threaded Mode is not enabled. "+
				"The bot owner must open the BotFather Mini App (search 'botfather' in "+
				"Telegram, tap Open on the search result) -> My bots -> this bot -> Bot "+
				"Settings -> Threads Settings -> enable Threaded Mode. This cannot be enabled "+
				"from the DM chat, nor from the BotFather /mybots text menu.", a.Name(), name, chatID))
		default:
			slog.Warn(fmt.Sprintf("[%s] Failed to create DM topic '%s' in chat %d: %s", a.Name(), name, chatID, redactTelegramErrorText(err)))
		}
		return nil, nil
	}
	threadID := int64(topic.MessageThreadID)
	slog.Info(fmt.Sprintf("[%s] Created DM topic '%s' in chat %d -> thread_id=%d", a.Name(), name, chatID, threadID))
	return &threadID, nil
}

// Creates a forum topic for a session handoff, ok false when none was created
func (a *TelegramAdapter) CreateHandoffThread(ctx context.Context, parentChatID, name string) (string, bool, error) {
	chatID, err := strconv.ParseInt(pystr.Strip(parentChatID), 10, 64)
	if err != nil {
		return "", false, nil
	}
	threadID, err := a.CreateDMTopic(ctx, chatID, name, nil, nil)
	if err != nil || threadID == nil || *threadID == 0 {
		return "", false, err
	}
	return strconv.FormatInt(*threadID, 10), true, nil
}

// Config entry of a chat in the DM topics config and its topic of the name, nil when absent
func (a *TelegramAdapter) dmTopicConfig(chatID string, name string) (map[string]any, map[string]any) {
	for _, raw := range a.dmTopicsConfig {
		entry, ok := raw.(map[string]any)
		if !ok || pyrepr.Str(entry["chat_id"]) != chatID {
			continue
		}
		topics, _ := entry["topics"].([]any)
		for _, t := range topics {
			if topic, ok := t.(map[string]any); ok && topic["name"] == name {
				return entry, topic
			}
		}
		return entry, nil
	}
	return nil, nil
}

// Integer of a config value, ok false when it is not an integer
func configInt(v any) (int64, bool) {
	switch t := v.(type) {
	case int:
		return int64(t), true
	case int64:
		return t, true
	case float64:
		return int64(t), t == float64(int64(t))
	case string:
		n, err := strconv.ParseInt(pystr.Strip(t), 10, 64)
		return n, err == nil
	}
	return 0, false
}

// Private DM topic thread id, creating and persisting it when needed
func (a *TelegramAdapter) EnsureDMTopic(ctx context.Context, chatID, name string, forceCreate bool) (string, error) {
	name = pystr.Strip(name)
	if name == "" {
		return "", nil
	}
	chatIDInt, err := strconv.ParseInt(pystr.Strip(chatID), 10, 64)
	if err != nil {
		return "", nil
	}
	cacheKey := fmt.Sprintf("%d:%s", chatIDInt, name)
	a.mu.Lock()
	cached, ok := a.dmTopics.Get(cacheKey)
	a.mu.Unlock()
	if ok && cached != 0 && !forceCreate {
		return strconv.FormatInt(cached, 10), nil
	}
	a.mu.Lock()
	chatEntry, topicConf := a.dmTopicConfig(strconv.FormatInt(chatIDInt, 10), name)
	if topicConf != nil && pyTruthy(topicConf["thread_id"]) && !forceCreate {
		threadID, _ := configInt(topicConf["thread_id"])
		a.dmTopics.Set(cacheKey, threadID)
		a.mu.Unlock()
		return strconv.FormatInt(threadID, 10), nil
	}
	if chatEntry == nil {
		chatEntry = map[string]any{"chat_id": chatIDInt, "topics": []any{}}
		a.dmTopicsConfig = append(a.dmTopicsConfig, chatEntry)
	}
	if topicConf == nil {
		topicConf = map[string]any{"name": name}
		topics, _ := chatEntry["topics"].([]any)
		chatEntry["topics"] = append(topics, topicConf)
	}
	a.mu.Unlock()
	var iconColor *int
	if c, ok := configInt(topicConf["icon_color"]); ok {
		ci := int(c)
		iconColor = &ci
	}
	var emoji *string
	if v, ok := topicConf["icon_custom_emoji_id"]; ok && v != nil {
		s := pyrepr.Str(v)
		emoji = &s
	}
	threadID, err := a.CreateDMTopic(ctx, chatIDInt, name, iconColor, emoji)
	if err != nil || threadID == nil || *threadID == 0 {
		return "", err
	}
	a.mu.Lock()
	topicConf["thread_id"] = *threadID
	a.dmTopics.Set(cacheKey, *threadID)
	a.mu.Unlock()
	a.persistDMTopicThreadID(ctx, chatIDInt, name, *threadID, forceCreate)
	return strconv.FormatInt(*threadID, 10), nil
}

// Renames a forum topic in a private chat
func (a *TelegramAdapter) RenameDMTopic(ctx context.Context, chatID string, threadID *big.Int, title string) error {
	b := a.currentBot()
	if b == nil {
		return nil
	}
	var chatArg any = chatID
	if n, err := strconv.ParseInt(pystr.Strip(chatID), 10, 64); err == nil {
		chatArg = n
	}
	if !threadID.IsInt64() {
		return &valueError{msg: "thread id out of range"}
	}
	if _, err := b.EditForumTopic(ctx, &bot.EditForumTopicParams{ChatID: chatArg, MessageThreadID: int(threadID.Int64()), Name: title}); err != nil {
		return ptbError(err)
	}
	slog.Info(fmt.Sprintf("[%s] Renamed DM topic in chat %s thread_id=%s -> '%s'", a.Name(), chatID, threadID.String(), title))
	return nil
}

// Saves a created thread id back into config.yaml so it survives restarts
func (a *TelegramAdapter) persistDMTopicThreadID(ctx context.Context, chatID int64, topicName string, threadID int64, replaceExisting bool) {
	h := home.Dir(ctx)
	configPath := filepath.Join(h, "config.yaml")
	err := func() error {
		exists, err := config.ConfigExists(ctx, configPath)
		if err != nil {
			return err
		}
		if !exists {
			slog.Warn(fmt.Sprintf("[%s] Config file not found at %s, cannot persist thread_id", a.Name(), configPath))
			return nil
		}
		cfg, err := config.ReadUserConfigRaw(ctx, configPath)
		if err != nil {
			return err
		}
		platforms := setDefaultMap(cfg, "platforms")
		telegramCfg := setDefaultMap(platforms, "telegram")
		extra := setDefaultMap(telegramCfg, "extra")
		dmTopics, _ := extra["dm_topics"].([]any)
		if _, has := extra["dm_topics"]; !has {
			dmTopics = []any{}
		}
		changed := false
		var matching map[string]any
		for _, raw := range dmTopics {
			entry, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			var entryChat any = 0
			if v, has := entry["chat_id"]; has {
				entryChat = v
			}
			n, ok := configInt(entryChat)
			if !ok || n != chatID {
				continue
			}
			matching = entry
			topics, _ := entry["topics"].([]any)
			if _, has := entry["topics"]; !has {
				topics = []any{}
			}
			var found map[string]any
			for _, t := range topics {
				if topic, ok := t.(map[string]any); ok && topic["name"] == topicName {
					found = topic
					break
				}
			}
			if found == nil {
				topics = append(topics, map[string]any{"name": topicName, "thread_id": threadID})
				changed = true
			} else if existing, has := configInt(found["thread_id"]); (replaceExisting || !pyTruthy(found["thread_id"])) && !(has && existing == threadID) {
				found["thread_id"] = threadID
				changed = true
			}
			entry["topics"] = topics
			break
		}
		if matching == nil {
			dmTopics = append(dmTopics, map[string]any{"chat_id": chatID, "topics": []any{map[string]any{"name": topicName, "thread_id": threadID}}})
			changed = true
		}
		extra["dm_topics"] = dmTopics
		if !changed {
			return nil
		}
		if err := config.AtomicConfigWrite(ctx, h, configPath, cfg, ""); err != nil {
			return err
		}
		slog.Info(fmt.Sprintf("[%s] Persisted thread_id=%d for topic '%s' in config.yaml", a.Name(), threadID, topicName))
		return nil
	}()
	if err != nil {
		slog.Warn(fmt.Sprintf("[%s] Failed to persist thread_id to config: %s", a.Name(), pyrepr.ExceptionMessage(err)))
	}
}

// Nested map of a key, created when absent, the Python dict setdefault
func setDefaultMap(m map[string]any, key string) map[string]any {
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	if _, ok := m[key]; ok {
		return map[string]any{}
	}
	created := map[string]any{}
	m[key] = created
	return created
}

// Loads configured DM topic thread ids or creates the missing topics with a seed message
func (a *TelegramAdapter) setupDMTopics(ctx context.Context) error {
	a.mu.Lock()
	entries := append([]any(nil), a.dmTopicsConfig...)
	a.mu.Unlock()
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		chatID := entry["chat_id"]
		topics, _ := entry["topics"].([]any)
		if !pyTruthy(chatID) || len(topics) == 0 {
			continue
		}
		slog.Info(fmt.Sprintf("[%s] Setting up %d DM topic(s) for chat %s", a.Name(), len(topics), pyrepr.Str(chatID)))
		for _, t := range topics {
			topicConf, ok := t.(map[string]any)
			if !ok {
				continue
			}
			topicName := pyrepr.Str(pyrepr.Or(topicConf["name"], ""))
			if !pyTruthy(topicConf["name"]) {
				continue
			}
			cacheKey := pyrepr.Str(chatID) + ":" + topicName
			if existing := topicConf["thread_id"]; pyTruthy(existing) {
				n, ok := configInt(existing)
				if !ok {
					return &valueError{msg: "invalid literal for int() with base 10: " + pyrepr.Repr(existing)}
				}
				a.mu.Lock()
				a.dmTopics.Set(cacheKey, n)
				a.mu.Unlock()
				slog.Info(fmt.Sprintf("[%s] DM topic loaded from config: %s -> thread_id=%s", a.Name(), cacheKey, pyrepr.Str(existing)))
				continue
			}
			normalized, ok := configInt(NormalizeTelegramChatID(chatID))
			if !ok {
				return &valueError{msg: "invalid literal for int() with base 10: " + pyrepr.Repr(chatID)}
			}
			var iconColor *int
			if c, ok := configInt(topicConf["icon_color"]); ok {
				ci := int(c)
				iconColor = &ci
			}
			var emoji *string
			if v, ok := topicConf["icon_custom_emoji_id"]; ok && v != nil {
				s := pyrepr.Str(v)
				emoji = &s
			}
			threadID, err := a.CreateDMTopic(ctx, normalized, topicName, iconColor, emoji)
			if err != nil {
				return err
			}
			if threadID == nil || *threadID == 0 {
				continue
			}
			a.mu.Lock()
			a.dmTopics.Set(cacheKey, *threadID)
			a.mu.Unlock()
			slog.Info(fmt.Sprintf("[%s] DM topic cached: %s -> thread_id=%d", a.Name(), cacheKey, *threadID))
			a.persistDMTopicThreadID(ctx, normalized, topicName, *threadID, false)
			thread := int(*threadID)
			_, seedErr := a.boundedSendMessage(ctx, &bot.SendMessageParams{ChatID: normalized, MessageThreadID: thread, Text: "\U0001f4cc " + topicName})
			if seedErr != nil {
				slog.Debug(fmt.Sprintf("[%s] Could not send seed message to topic '%s': %s", a.Name(), topicName, pyrepr.ExceptionMessage(seedErr)))
			}
		}
	}
	return nil
}

// Re-reads dm_topics from config.yaml so externally created topics resolve without a restart
func (a *TelegramAdapter) reloadDMTopicsFromConfig(ctx context.Context) {
	tree, err := config.LoadReadonly(ctx, home.Dir(ctx), nil)
	if err != nil {
		slog.Debug(fmt.Sprintf("[%s] Failed to reload dm_topics from config: %s", a.Name(), pyrepr.ExceptionMessage(err)))
		return
	}
	platforms, _ := tree["platforms"].(map[string]any)
	telegramCfg, _ := platforms["telegram"].(map[string]any)
	extra, _ := telegramCfg["extra"].(map[string]any)
	dmTopics, _ := extra["dm_topics"].([]any)
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(dmTopics) == 0 {
		a.dmTopicsConfig, a.dmTopicChatIDs = []any{}, map[string]bool{}
		return
	}
	a.dmTopicsConfig = dmTopics
	a.dmTopicChatIDs = map[string]bool{}
	for _, raw := range dmTopics {
		if entry, ok := raw.(map[string]any); ok {
			if cid, has := entry["chat_id"]; has {
				a.dmTopicChatIDs[pyrepr.Str(cid)] = true
			}
		}
	}
	for _, raw := range dmTopics {
		entry, ok := raw.(map[string]any)
		if !ok || !pyTruthy(entry["chat_id"]) {
			continue
		}
		cid := pyrepr.Str(entry["chat_id"])
		topics, _ := entry["topics"].([]any)
		for _, t := range topics {
			topic, ok := t.(map[string]any)
			if !ok || !pyTruthy(topic["thread_id"]) || !pyTruthy(topic["name"]) {
				continue
			}
			key := cid + ":" + pyrepr.Str(topic["name"])
			if a.dmTopics.Has(key) {
				continue
			}
			n, ok := configInt(topic["thread_id"])
			if !ok {
				slog.Debug(fmt.Sprintf("[%s] Failed to reload dm_topics from config: %s", a.Name(),
					"invalid literal for int() with base 10: "+pyrepr.Repr(topic["thread_id"])))
				return
			}
			a.dmTopics.Set(key, n)
			slog.Info(fmt.Sprintf("[%s] Hot-loaded DM topic from config: %s -> thread_id=%s", a.Name(), key, pyrepr.Str(topic["thread_id"])))
		}
	}
}

// DM topic config of a thread, reloading the config once on a cache miss
func (a *TelegramAdapter) getDMTopicInfo(ctx context.Context, chatID, threadID string) map[string]any {
	info, _ := a.dmTopicInfo(ctx, chatID, threadID)
	return info
}

// DM topic config of a thread or a ValueError for a non numeric thread id
func (a *TelegramAdapter) dmTopicInfo(ctx context.Context, chatID, threadID string) (map[string]any, error) {
	if threadID == "" {
		return nil, nil
	}
	tid, err := strconv.ParseInt(pystr.Strip(threadID), 10, 64)
	if err != nil {
		return nil, &valueError{msg: "invalid literal for int() with base 10: " + pyrepr.Repr(threadID)}
	}
	lookup := func() map[string]any {
		a.mu.Lock()
		defer a.mu.Unlock()
		for key, cached := range a.dmTopics.All() {
			if cached != tid || !strings.HasPrefix(key, chatID+":") {
				continue
			}
			topicName := key[strings.Index(key, ":")+1:]
			for _, raw := range a.dmTopicsConfig {
				entry, ok := raw.(map[string]any)
				if !ok || pyrepr.Str(entry["chat_id"]) != chatID {
					continue
				}
				topics, _ := entry["topics"].([]any)
				for _, t := range topics {
					if topic, ok := t.(map[string]any); ok && topic["name"] == topicName {
						return topic
					}
				}
			}
			return map[string]any{"name": topicName}
		}
		return nil
	}
	if found := lookup(); found != nil {
		return found, nil
	}
	a.reloadDMTopicsFromConfig(ctx)
	return lookup(), nil
}

// DM topic config of a thread for the runner's thread metadata
func (a *TelegramAdapter) GetDMTopicInfo(chatID, threadID string) (any, error) {
	info, err := a.dmTopicInfo(home.WithOverride(context.Background(), a.homeDir), chatID, threadID)
	if err != nil || info == nil {
		return nil, err
	}
	return info, nil
}

// Caches a thread to topic name mapping discovered from an incoming message
func (a *TelegramAdapter) cacheDMTopicFromMessage(chatID, threadID, topicName string) {
	key := chatID + ":" + topicName
	tid, err := strconv.ParseInt(threadID, 10, 64)
	if err != nil {
		return
	}
	a.mu.Lock()
	if a.dmTopics.Has(key) {
		a.mu.Unlock()
		return
	}
	a.dmTopics.Set(key, tid)
	a.mu.Unlock()
	slog.Info(fmt.Sprintf("[%s] Cached DM topic from message: %s -> thread_id=%s", a.Name(), key, threadID))
}
