package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Seconds of a Retry-After value or header set, numeric or HTTP date, clamped at zero
func parseRetryAfterSeconds(raw any) (float64, bool) {
	if h, ok := raw.(http.Header); ok {
		v := h.Get("Retry-After")
		if v == "" {
			return 0, false
		}
		raw = v
	}
	switch v := raw.(type) {
	case nil, bool:
		return 0, false
	case float64:
		return math.Max(0, v), true
	case int:
		return math.Max(0, float64(v)), true
	case string:
		text := pystr.Strip(v)
		if text == "" {
			return 0, false
		}
		if f, err := pyint.ParseFloat(text); err == nil {
			return math.Max(0, f), true
		}
		when, err := http.ParseTime(text)
		if err != nil {
			return 0, false
		}
		return math.Max(0, time.Until(when).Seconds()), true
	}
	return 0, false
}

// Seconds to wait after a 429, the rate limit hint or response headers, floored at one second
func extractDiscordRetryAfter(err error) *float64 {
	floor := func(v float64) *float64 {
		v = math.Max(1.0, v)
		return &v
	}
	var rl *discordgo.RateLimitError
	if errors.As(err, &rl) && rl.RateLimit != nil {
		if v, ok := parseRetryAfterSeconds(rl.RetryAfter.Seconds()); ok {
			return floor(v)
		}
		return nil
	}
	var rest *discordgo.RESTError
	if !errors.As(err, &rest) || rest.Response == nil || len(rest.Response.Header) == 0 {
		return nil
	}
	v, ok := parseRetryAfterSeconds(rest.Response.Header)
	if !ok {
		if v, ok = parseRetryAfterSeconds(rest.Response.Header.Get("X-RateLimit-Reset-After")); !ok {
			return nil
		}
	}
	return floor(v)
}

// Running typing loop of one chat
type typingTask struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// Starts a persistent typing loop posting every 12 seconds, sleeping out rate limits
func (a *DiscordAdapter) SendTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	client := a.liveClient()
	if client == nil {
		return nil
	}
	a.mu.Lock()
	if _, running := a.typingTasks[chatID]; running {
		a.mu.Unlock()
		return nil
	}
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	task := &typingTask{cancel: cancel, done: make(chan struct{})}
	a.typingTasks[chatID] = task
	a.mu.Unlock()
	go func() {
		defer close(task.done)
		defer func() {
			a.mu.Lock()
			if a.typingTasks[chatID] == task {
				delete(a.typingTasks, chatID)
			}
			a.mu.Unlock()
		}()
		for {
			wait := 12.0
			if err := client.ChannelTyping(chatID, discordgo.WithContext(loopCtx)); err != nil {
				if loopCtx.Err() != nil {
					return
				}
				retryAfter := extractDiscordRetryAfter(err)
				if retryAfter == nil {
					slog.Debug(fmt.Sprintf("Discord typing indicator failed for %s: %s", chatID, dpyError(err)))
					return
				}
				slog.Warn(fmt.Sprintf("Typing indicator rate-limited for %s; retrying in %.1fs", chatID, *retryAfter))
				wait = *retryAfter
			}
			select {
			case <-loopCtx.Done():
				return
			case <-time.After(time.Duration(wait * float64(time.Second))):
			}
		}
	}()
	return nil
}

// Stops the persistent typing indicator of a channel and waits for its loop to exit
func (a *DiscordAdapter) StopTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	a.mu.Lock()
	task := a.typingTasks[chatID]
	delete(a.typingTasks, chatID)
	a.mu.Unlock()
	if task != nil {
		task.cancel()
		<-task.done
	}
	return nil
}

// Name, type and guild of a Discord channel, reported as a DM when disconnected or failing
func (a *DiscordAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	client := a.liveClient()
	if client == nil {
		return map[string]any{"name": "Unknown", "type": "dm"}, nil
	}
	ch, err := a.resolveChannel(ctx, chatID)
	if err != nil {
		slog.Error(fmt.Sprintf("[%s] Failed to get chat info for %s: %s", a.Name(), chatID, err))
		return map[string]any{"name": chatID, "type": "dm", "error": err.Error()}, nil
	}
	if ch == nil {
		return map[string]any{"name": chatID, "type": "dm"}, nil
	}
	var guild *discordgo.Guild
	if ch.GuildID != "" && client.State != nil {
		guild, _ = client.State.Guild(ch.GuildID)
	}
	var chatType, name string
	switch {
	case ch.Type == discordgo.ChannelTypeDM:
		chatType, name = "dm", chatID
		if len(ch.Recipients) > 0 && ch.Recipients[0] != nil {
			name = ch.Recipients[0].Username
		}
	case ch.IsThread():
		chatType, name = "thread", ch.Name
	case ch.Type == discordgo.ChannelTypeGuildText || ch.Type == discordgo.ChannelTypeGuildNews:
		chatType, name = "channel", "#"+ch.Name
		if guild != nil {
			name = guild.Name + " / " + name
		}
	default:
		chatType, name = "channel", ch.Name
	}
	info := map[string]any{"name": name, "type": chatType, "guild_id": nil, "guild_name": nil}
	if guild != nil {
		info["guild_id"], info["guild_name"] = guild.ID, guild.Name
	}
	return info, nil
}

// Every member of a guild, the cached list when complete else paged from the REST API
func guildMembers(ctx context.Context, client *discordgo.Session, g *discordgo.Guild) ([]*discordgo.Member, error) {
	if len(g.Members) >= g.MemberCount {
		return g.Members, nil
	}
	var out []*discordgo.Member
	after := ""
	for {
		page, err := client.GuildMembers(g.ID, after, 1000, discordgo.WithContext(ctx))
		if err != nil {
			return nil, dpyError(err)
		}
		out = append(out, page...)
		if len(page) < 1000 {
			return out, nil
		}
		after = page[len(page)-1].User.ID
	}
}

// Resolves username allowlist entries to numeric ids by account username only
func (a *DiscordAdapter) resolveAllowedUsernames(ctx context.Context) {
	a.mu.Lock()
	a.usernameResolvedIDs = strSet{}
	allowed := a.allowedUserIDs
	a.mu.Unlock()
	client := a.liveClient()
	if len(allowed) == 0 || client == nil {
		return
	}
	numericIDs, toResolve := strSet{}, strSet{}
	for entry := range allowed {
		if isDigitString(entry) || entry == "*" {
			numericIDs[entry] = struct{}{}
		} else {
			toResolve[strings.ToLower(entry)] = struct{}{}
		}
	}
	if len(toResolve) == 0 {
		return
	}
	fmt.Printf("[%s] Resolving %d username(s): %s\n", a.Name(), len(toResolve), strings.Join(keysOf(toResolve), ", "))
	displayOnly := strSet{}
	resolved := strSet{}
	for _, g := range client.State.Guilds {
		members, err := guildMembers(ctx, client, g)
		if err != nil {
			slog.Warn(fmt.Sprintf("Failed to fetch members for guild %s: %s", g.Name, err))
			continue
		}
		for _, m := range members {
			if m.User == nil {
				continue
			}
			nameLower := strings.ToLower(m.User.Username)
			if toResolve.has(nameLower) {
				uid := m.User.ID
				numericIDs[uid] = struct{}{}
				resolved[uid] = struct{}{}
				delete(toResolve, nameLower)
				fmt.Printf("[%s] Resolved '%s' -> %s (%s#%s)\n", a.Name(), nameLower, uid, m.User.Username, m.User.Discriminator)
				continue
			}
			for _, shown := range []string{memberDisplayName(m), m.User.GlobalName} {
				if shown != "" && toResolve.has(strings.ToLower(shown)) {
					displayOnly[strings.ToLower(shown)] = struct{}{}
				}
			}
		}
		if len(toResolve) == 0 {
			break
		}
	}
	if len(toResolve) > 0 {
		fmt.Printf("[%s] Could not resolve usernames: %s\n", a.Name(), strings.Join(keysOf(toResolve), ", "))
		var both []string
		for entry := range toResolve {
			if displayOnly.has(entry) {
				both = append(both, entry)
			}
		}
		sort.Strings(both)
		for _, entry := range both {
			fmt.Printf("[%s] '%s' matches only a display name or server nickname, which any member can set; allowlist that member's username or numeric user ID instead\n", a.Name(), entry)
		}
	}
	sorted := keysOf(numericIDs)
	sort.Strings(sorted)
	joined := strings.Join(sorted, ",")
	a.mu.Lock()
	a.allowedUserIDs = numericIDs
	a.usernameResolvedIDs = resolved
	if a.gateEnvSnapshot != nil {
		a.gateEnvSnapshot["DISCORD_ALLOWED_USERS"] = joined
	}
	a.mu.Unlock()
	if !tools.MultiplexActive() {
		os.Setenv("DISCORD_ALLOWED_USERS", joined)
	}
	if len(resolved) > 0 {
		fmt.Printf("[%s] Updated DISCORD_ALLOWED_USERS with %d resolved ID(s)\n", a.Name(), len(resolved))
	}
}

// Server nickname, else global name, else account username
func memberDisplayName(m *discordgo.Member) string {
	if m.Nick != "" {
		return m.Nick
	}
	if m.User == nil {
		return ""
	}
	if m.User.GlobalName != "" {
		return m.User.GlobalName
	}
	return m.User.Username
}
