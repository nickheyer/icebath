package discord

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/state"
)

// Background goroutine the adapter owns, cancelled through its context and awaited on done
type bgTask struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// Starts fn on a goroutine under a cancellable child of ctx
func spawnTask(ctx context.Context, fn func(ctx context.Context)) *bgTask {
	taskCtx, cancel := context.WithCancel(ctx)
	t := &bgTask{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(t.done)
		fn(taskCtx)
	}()
	return t
}

// Whether the goroutine has returned
func (t *bgTask) isDone() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

// Cancels the goroutine and waits for it to return
func (t *bgTask) stop() {
	t.cancel()
	<-t.done
}

// Flags and name view of a channel for the hidden channel check
type obfuscationView struct{ ch *discordgo.Channel }

func (v obfuscationView) DiscordChannelFlags() (any, bool) { return int64(v.ch.Flags), true }
func (v obfuscationView) DiscordChannelName() (any, bool)  { return v.ch.Name, true }

// Discord snowflake of a timestamp, the high variant filling the increment bits
func timeSnowflake(t time.Time, high bool) string {
	millis := t.UnixMilli() - 1420070400000
	n := uint64(millis) << 22
	if high {
		n += 1<<22 - 1
	}
	return strconv.FormatUint(n, 10)
}

// The running recovery task, a new one when none runs, joined to the startup restore gate
func (a *DiscordAdapter) ensureMissedMessageBackfillTask(ctx context.Context) *bgTask {
	a.mu.Lock()
	task := a.missedMessageBackfillTask
	if task != nil && !task.isDone() {
		a.mu.Unlock()
		return task
	}
	task = spawnTask(context.WithoutCancel(ctx), a.runMissedMessageBackfill)
	a.missedMessageBackfillTask = task
	a.mu.Unlock()
	if joiner, ok := a.GatewayRunner.(gateway.StartupRestoreTaskJoiner); ok {
		joiner.JoinStartupRestore(ctx, "discord-missed-message-backfill", func(ctx context.Context) error {
			select {
			case <-task.done:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}
	return task
}

// Records the end of a recovery scan with its counts
func (a *DiscordAdapter) finishRecoveryScan(ctx context.Context, scanID, status string, counts map[string]int, errText *string) {
	a.recordRecoveryScanComplete(context.WithoutCancel(ctx), scanID, status, counts["scanned"], counts["missed"], counts["dispatched"], errText)
}

// Re-dispatches recent messages missed while the bot was down that lack a substantive bot response
func (a *DiscordAdapter) runMissedMessageBackfill(ctx context.Context) {
	if a.liveClient() == nil {
		return
	}
	channels := a.missedMessageBackfillChannels(ctx)
	ledgerOK := a.withDiscordRecoveryDB(ctx, func(conn state.SQLiteConn) (any, error) {
		var one int
		if err := conn.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
			return false, nil
		}
		return true, nil
	}, false)
	if ok, _ := ledgerOK.(bool); !ok {
		slog.Error(fmt.Sprintf("[%s] Missed-message recovery aborted: durable ledger unavailable", a.Name()))
		return
	}
	scanID := a.recordRecoveryScanStart(ctx, channels)
	counts := map[string]int{"scanned": 0, "missed": 0, "dispatched": 0}
	if len(channels) == 0 {
		slog.Info(fmt.Sprintf("[%s] Missed-message backfill enabled but no channels configured", a.Name()))
		a.finishRecoveryScan(ctx, scanID, "skipped", counts, nil)
		return
	}
	maxDispatches := a.missedMessageBackfillMaxDispatches(ctx)
	var failure error
	for m := range a.iterMissedMessageBackfillCandidates(ctx, channels) {
		if ctx.Err() != nil {
			break
		}
		counts["scanned"]++
		a.recordDiscordMessageSeen(ctx, m, "discovered")
		if a.dedup.Contains(m.ID) {
			continue
		}
		if !a.shouldBackfillDiscordMessage(ctx, m) {
			continue
		}
		counts["missed"]++
		slog.Info(fmt.Sprintf("[%s] Backfilling missed Discord message %s in channel %s", a.Name(), m.ID, m.ChannelID))
		a.recordRecoveryAttempt(ctx, m, "queued", nil)
		admitted, err := a.dispatchRecoveredMessage(ctx, m)
		if err == nil && ctx.Err() != nil {
			err = ctx.Err()
		}
		if err != nil {
			a.dedup.Discard(m.ID)
			if errors.Is(err, context.Canceled) {
				a.recordRecoveryAttempt(context.WithoutCancel(ctx), m, "cancelled", nil)
			} else {
				text := err.Error()
				a.recordRecoveryAttempt(ctx, m, "failed", &text)
			}
			failure = err
			break
		}
		if admitted {
			counts["dispatched"]++
		}
		if counts["dispatched"] >= maxDispatches {
			break
		}
	}
	if failure == nil && ctx.Err() != nil {
		failure = ctx.Err()
	}
	switch {
	case failure == nil:
		a.finishRecoveryScan(ctx, scanID, "success", counts, nil)
		slog.Info(fmt.Sprintf("[%s] Missed-message backfill complete: scanned=%d missed=%d dispatched=%d", a.Name(), counts["scanned"], counts["missed"], counts["dispatched"]))
	case errors.Is(failure, context.Canceled):
		a.finishRecoveryScan(ctx, scanID, "cancelled", counts, nil)
	default:
		text := failure.Error()
		a.finishRecoveryScan(ctx, scanID, "failed", counts, &text)
		slog.Warn(fmt.Sprintf("[%s] Missed-message backfill failed: %s", a.Name(), failure))
	}
}

// Runs one recovered message through the live ingress gates without claiming its id
func (a *DiscordAdapter) dispatchRecoveredMessage(ctx context.Context, m *inboundMessage) (bool, error) {
	if !isDMChannel(m.channel) {
		parentID := a.getParentChannelID(m.channel)
		keys := a.discordChannelKeys(m, parentID)
		free := a.discordFreeResponseChannels(ctx)
		if a.discordRequireMention(ctx) && !free.has("*") && !keys.intersects(free) && !a.inBotThread(ctx, m) && !a.selfIsExplicitlyMentioned(m) {
			return false, nil
		}
	}
	admitted, roleAuthorized := a.discordMessageAdmission(ctx, m, false)
	if !admitted {
		return false, nil
	}
	return a.handleDiscordMessage(ctx, m, roleAuthorized, true), nil
}

// Text channels of every cached guild in position order
func guildTextChannels(g *discordgo.Guild) []*discordgo.Channel {
	var out []*discordgo.Channel
	for _, ch := range g.Channels {
		if ch.Type == discordgo.ChannelTypeGuildText || ch.Type == discordgo.ChannelTypeGuildNews {
			out = append(out, ch)
		}
	}
	slices.SortStableFunc(out, func(x, y *discordgo.Channel) int {
		if x.Position != y.Position {
			return x.Position - y.Position
		}
		xi, _ := strconv.ParseUint(x.ID, 10, 64)
		yi, _ := strconv.ParseUint(y.ID, 10, 64)
		switch {
		case xi < yi:
			return -1
		case xi > yi:
			return 1
		}
		return 0
	})
	return out
}

// Recent messages of the configured channels and their threads, one per channel per round up to the limit
func (a *DiscordAdapter) iterMissedMessageBackfillCandidates(ctx context.Context, channelIDs strSet) iter.Seq[*inboundMessage] {
	return func(yield func(*inboundMessage) bool) {
		client := a.liveClient()
		if client == nil {
			return
		}
		after := time.Now().UTC().Add(-time.Duration(a.missedMessageBackfillWindowSeconds(ctx) * float64(time.Second)))
		limit := a.missedMessageBackfillLimit(ctx)
		seen := map[string]bool{}
		var candidates []*discordgo.Channel
		if channelIDs.has("*") {
			if client.State != nil {
				for _, g := range client.State.Guilds {
					candidates = append(candidates, guildTextChannels(g)...)
				}
			}
		} else {
			for _, id := range sortedChannelIDs(channelIDs) {
				ch := a.stateChannel(id)
				if ch == nil {
					fetched, err := client.Channel(id, discordgo.WithContext(ctx))
					if err != nil {
						slog.Debug(fmt.Sprintf("[%s] Cannot fetch backfill channel %s: %s", a.Name(), id, dpyError(err)))
						continue
					}
					ch = fetched
				}
				candidates = append(candidates, ch)
			}
		}
		candidates = slices.DeleteFunc(candidates, func(ch *discordgo.Channel) bool { return gateway.IsDiscordChannelObfuscated(obfuscationView{ch}) })
		type puller struct {
			next func() (*inboundMessage, bool)
			stop func()
		}
		var iterators []puller
		for _, ch := range candidates {
			next, stop := iter.Pull(a.iterChannelAndThreadMessages(ctx, ch, limit, after, seen))
			iterators = append(iterators, puller{next, stop})
		}
		defer func() {
			for _, it := range iterators {
				it.stop()
			}
		}()
		yielded := 0
		for len(iterators) > 0 && yielded < limit {
			var nextRound []puller
			for _, it := range iterators {
				m, ok := it.next()
				if !ok {
					continue
				}
				if !yield(m) {
					return
				}
				yielded++
				nextRound = append(nextRound, it)
				if yielded >= limit {
					return
				}
			}
			iterators = nextRound
		}
	}
}

// Channel ids in Python's sorted string order
func sortedChannelIDs(ids strSet) []string {
	out := keysOf(ids)
	slices.Sort(out)
	return out
}

// Archived public threads of a channel, newest first, paged up to limit
func (a *DiscordAdapter) archivedThreads(ctx context.Context, channelID string, limit int) ([]*discordgo.Channel, error) {
	client := a.liveClient()
	if client == nil {
		return nil, errors.New("Not connected")
	}
	var out []*discordgo.Channel
	before := ""
	for limit > 0 {
		q := url.Values{"limit": {strconv.Itoa(min(limit, 100))}}
		if before != "" {
			q.Set("before", before)
		}
		endpoint := discordgo.EndpointChannelPublicArchivedThreads(channelID)
		body, err := client.RequestWithBucketID(http.MethodGet, endpoint+"?"+q.Encode(), nil, endpoint, discordgo.WithContext(ctx))
		if err != nil {
			return out, dpyError(err)
		}
		var page discordgo.ThreadsList
		if err := discordgo.Unmarshal(body, &page); err != nil {
			return out, err
		}
		out = append(out, page.Threads...)
		limit -= len(page.Threads)
		if !page.HasMore || len(page.Threads) == 0 {
			break
		}
		last := page.Threads[len(page.Threads)-1]
		if last.ThreadMetadata == nil {
			break
		}
		before = last.ThreadMetadata.ArchiveTimestamp.UTC().Format(time.RFC3339Nano)
	}
	return out, nil
}

// History of a channel in chronological order then its active and archived threads, each id walked once
func (a *DiscordAdapter) iterChannelAndThreadMessages(ctx context.Context, ch *discordgo.Channel, limit int, after time.Time, seen map[string]bool) iter.Seq[*inboundMessage] {
	return func(yield func(*inboundMessage) bool) {
		a.walkChannelAndThreads(ctx, ch, limit, after, seen, yield)
	}
}

// Yields a channel history and recurses into its threads, false once the consumer stops
func (a *DiscordAdapter) walkChannelAndThreads(ctx context.Context, ch *discordgo.Channel, limit int, after time.Time, seen map[string]bool, yield func(*inboundMessage) bool) bool {
	key := ch.ID
	if key == "" || seen[key] {
		return true
	}
	seen[key] = true
	channelAfter := timeSnowflake(after, true)
	if cursor := a.discordRecoveryCursor(ctx, key); cursor != "" {
		if id, err := strconv.ParseUint(cursor, 10, 64); err == nil && discordSnowflakeTime(id).After(after) {
			channelAfter = cursor
		}
	}
	var messages []*discordgo.Message
	err := a.channelHistory(ctx, key, limit, "", channelAfter, func(m *discordgo.Message) bool {
		messages = append(messages, m)
		return true
	})
	if err != nil {
		slog.Debug(fmt.Sprintf("[%s] Cannot read history for %s: %s", a.Name(), key, err))
	} else {
		for i := len(messages) - 1; i >= 0; i-- {
			m := messages[i]
			if m.GuildID == "" {
				m.GuildID = ch.GuildID
			}
			if !yield(&inboundMessage{Message: m, channel: ch}) {
				return false
			}
		}
	}
	var children []*discordgo.Channel
	if g := a.stateGuild(ch.GuildID); g != nil {
		for _, th := range g.Threads {
			if th.ParentID == key {
				children = append(children, th)
			}
		}
	}
	if ch.Type == discordgo.ChannelTypeGuildText || ch.Type == discordgo.ChannelTypeGuildNews || ch.Type == discordgo.ChannelTypeGuildForum || ch.Type == discordgo.ChannelTypeGuildMedia {
		archived, err := a.archivedThreads(ctx, key, limit)
		if err != nil {
			slog.Debug(fmt.Sprintf("[%s] Cannot list archived threads for %s: %s", a.Name(), key, err))
		}
		children = append(children, archived...)
	}
	for _, th := range children {
		if th.ID == "" || seen[th.ID] {
			continue
		}
		if !a.walkChannelAndThreads(ctx, th, limit, after, seen, yield) {
			return false
		}
	}
	return true
}
