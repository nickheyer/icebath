package discord

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

var downNoticeRE = regexp.MustCompile(`\b(?:icebath|the agent|agent|the gateway|gateway|bmo)\s+(?:is|was|appears to be|is currently|was currently)\s+(?:down|offline|unavailable|not running)\b`)

// Current UTC time in Python isoformat
func utcNowISO() string { return isoformat(time.Now().UTC()) }

// Result of fn in the recovery ledger, def when the ledger is unavailable
func (a *DiscordAdapter) withDiscordRecoveryDB(ctx context.Context, fn func(conn state.SQLiteConn) (any, error), def any) any {
	return a.recoveryStore.Call(ctx, fn, def)
}

// Whether to reconcile Discord messages missed while the background service was down
func (a *DiscordAdapter) missedMessageBackfillEnabled(ctx context.Context) bool {
	return settings.DiscordMissedMessageBackfillEnabled.Get(a.sctx(ctx))
}

// Channels scanned for missed messages, allowed plus free response channels when none are named
func (a *DiscordAdapter) missedMessageBackfillChannels(ctx context.Context) strSet {
	if channels := a.settingSet(ctx, settings.DiscordMissedMessageBackfillChannels); len(channels) > 0 {
		return channels
	}
	out := a.getAllowedChannels(ctx)
	for k := range a.discordFreeResponseChannels(ctx) {
		out[k] = struct{}{}
	}
	return out
}

// Seconds back the missed message scan reaches, at least one minute
func (a *DiscordAdapter) missedMessageBackfillWindowSeconds(ctx context.Context) float64 {
	return math.Max(60, settings.DiscordMissedMessageBackfillWindowSeconds.Get(a.sctx(ctx)).Seconds())
}

// Messages read per channel by the missed message scan, 1 to 500
func (a *DiscordAdapter) missedMessageBackfillLimit(ctx context.Context) int {
	return min(max(settings.DiscordMissedMessageBackfillLimit.Get(a.sctx(ctx)), 1), 500)
}

// Messages one scan may dispatch, 1 to 100
func (a *DiscordAdapter) missedMessageBackfillMaxDispatches(ctx context.Context) int {
	return min(max(settings.DiscordMissedMessageBackfillMaxDispatches.Get(a.sctx(ctx)), 1), 100)
}

// Lifetime dispatch ceiling of one message regardless of completion state, 1 to 100
func (a *DiscordAdapter) missedMessageBackfillMaxAttempts(ctx context.Context) int {
	return min(max(settings.DiscordMissedMessageBackfillMaxAttempts.Get(a.sctx(ctx)), 1), 100)
}

// Channel id, thread id and parent id of a message, the thread set only inside threads
func messageChannelIDs(m *inboundMessage) (string, *string, *string) {
	if m.channel == nil {
		return "", nil, nil
	}
	channelID := m.channel.ID
	if m.channel.ParentID == "" {
		return channelID, nil, nil
	}
	parent := m.channel.ParentID
	return channelID, &channelID, &parent
}

// Upserts the ledger row of a seen message without downgrading completed rows or live claims
func (a *DiscordAdapter) recordDiscordMessageSeen(ctx context.Context, m *inboundMessage, status string) {
	if !a.missedMessageBackfillEnabled(ctx) || m.ID == "" {
		return
	}
	channelID, threadID, parentID := messageChannelIDs(m)
	authorID := ""
	if m.Author != nil {
		authorID = m.Author.ID
	}
	var createdText any
	if !m.Timestamp.IsZero() {
		createdText = isoformat(m.Timestamp)
	}
	now := utcNowISO()
	a.withDiscordRecoveryDB(ctx, func(conn state.SQLiteConn) (any, error) {
		var existingStatus, existingUpdated sql.NullString
		err := conn.QueryRowContext(ctx, "SELECT status, updated_at FROM discord_messages WHERE message_id=?", m.ID).Scan(&existingStatus, &existingUpdated)
		exists := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		keep := exists && (existingStatus.String == "responded" || status == "discovered")
		finalStatus, updatedAt := status, now
		if keep {
			finalStatus = existingStatus.String
			if existingUpdated.Valid && existingUpdated.String != "" {
				updatedAt = existingUpdated.String
			}
		}
		_, err = conn.ExecContext(ctx, `
                INSERT INTO discord_messages (message_id, channel_id, thread_id, parent_channel_id, author_id, created_at, status, updated_at)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?)
                ON CONFLICT(message_id) DO UPDATE SET
                    channel_id=excluded.channel_id,
                    thread_id=excluded.thread_id,
                    parent_channel_id=excluded.parent_channel_id,
                    author_id=excluded.author_id,
                    created_at=COALESCE(discord_messages.created_at, excluded.created_at),
                    status=?,
                    updated_at=excluded.updated_at
                `, m.ID, channelID, threadID, parentID, authorID, createdText, finalStatus, updatedAt, finalStatus)
		return nil, err
	}, nil)
}

// Records one recovery transition, counting dispatches on the queued transition
func (a *DiscordAdapter) recordRecoveryAttempt(ctx context.Context, m *inboundMessage, status string, errText *string) {
	if !a.missedMessageBackfillEnabled(ctx) {
		return
	}
	a.recordDiscordMessageSeen(ctx, m, status)
	if m.ID == "" {
		return
	}
	now := utcNowISO()
	dispatched := 0
	if status == "queued" {
		dispatched = 1
	}
	a.withDiscordRecoveryDB(ctx, func(conn state.SQLiteConn) (any, error) {
		_, err := conn.ExecContext(ctx, `
                UPDATE discord_messages
                   SET status=?, attempts=attempts+?, last_attempt_at=?, last_error=?, updated_at=?
                 WHERE message_id=?
                `, status, dispatched, now, errText, now, m.ID)
		return nil, err
	}, nil)
}

// Inbound Discord message an event carries, nil for synthetic events
func eventRawMessage(event *gateway.MessageEvent) *inboundMessage {
	if event == nil {
		return nil
	}
	m, _ := event.RawMessage.(*inboundMessage)
	return m
}

// Id of the event's raw Discord message, else the event message id
func eventMessageID(event *gateway.MessageEvent) string {
	if m := eventRawMessage(event); m != nil && m.ID != "" {
		return m.ID
	}
	if event != nil && event.MessageID != nil {
		return *event.MessageID
	}
	return ""
}

// Marks the inbound message processing with its emoji ack state
func (a *DiscordAdapter) recordDiscordProcessingStart(ctx context.Context, event *gateway.MessageEvent, emojiAck bool) {
	if !a.missedMessageBackfillEnabled(ctx) {
		return
	}
	if m := eventRawMessage(event); m != nil {
		a.recordDiscordMessageSeen(ctx, m, "processing")
	}
	messageID := eventMessageID(event)
	if messageID == "" {
		return
	}
	now := utcNowISO()
	ack := 0
	if emojiAck {
		ack = 1
	}
	a.withDiscordRecoveryDB(ctx, func(conn state.SQLiteConn) (any, error) {
		_, err := conn.ExecContext(ctx, "UPDATE discord_messages SET status='processing', emoji_ack=?, updated_at=? WHERE message_id=?", ack, now, messageID)
		return nil, err
	}, nil)
}

// Marks the inbound message responded on success, else cancelled or failed unless already responded
func (a *DiscordAdapter) recordDiscordProcessingComplete(ctx context.Context, event *gateway.MessageEvent, outcome gateway.ProcessingOutcome) {
	if !a.missedMessageBackfillEnabled(ctx) {
		return
	}
	messageID := eventMessageID(event)
	if messageID == "" {
		return
	}
	now := utcNowISO()
	if outcome == gateway.ProcessingSuccess {
		a.withDiscordRecoveryDB(ctx, func(conn state.SQLiteConn) (any, error) {
			_, err := conn.ExecContext(ctx, "UPDATE discord_messages SET status='responded', replied=1, updated_at=? WHERE message_id=?", now, messageID)
			return nil, err
		}, nil)
		return
	}
	status := "failed"
	if outcome == gateway.ProcessingCancelled {
		status = "cancelled"
	}
	a.withDiscordRecoveryDB(ctx, func(conn state.SQLiteConn) (any, error) {
		_, err := conn.ExecContext(ctx, "UPDATE discord_messages "+
			"SET status=CASE WHEN status='responded' THEN status ELSE ? END, "+
			"updated_at=? WHERE message_id=?", status, now, messageID)
		return nil, err
	}, nil)
}

// Records a send outcome against its visual or internal reply anchor and returns it unchanged
func (a *DiscordAdapter) recordResponseAsync(ctx context.Context, replyTo string, result gateway.SendResult, content string, final bool, metadata map[string]any) gateway.SendResult {
	ledgerReplyTo := replyTo
	if ledgerReplyTo == "" && metadata != nil && anyutil.IsTrue(metadata["reply_to_message_id"]) {
		ledgerReplyTo = anyutil.String(metadata["reply_to_message_id"])
	}
	a.recordDiscordResponse(ctx, ledgerReplyTo, result, final)
	return result
}

// Upserts the response state of the answered message and advances its channel cursor on completion
func (a *DiscordAdapter) recordDiscordResponse(ctx context.Context, replyTo string, result gateway.SendResult, final bool) {
	if !a.missedMessageBackfillEnabled(ctx) || replyTo == "" {
		return
	}
	now := utcNowISO()
	completed := final && result.Success
	status, flag := "failed", 0
	if completed {
		status, flag = "responded", 1
	}
	a.withDiscordRecoveryDB(ctx, func(conn state.SQLiteConn) (any, error) {
		_, err := conn.ExecContext(ctx, `
                INSERT INTO discord_messages (message_id, status, replied, outage_response, response_message_id, updated_at)
                VALUES (?, ?, ?, 0, ?, ?)
                ON CONFLICT(message_id) DO UPDATE SET
                    status=CASE WHEN ? THEN 'responded' ELSE discord_messages.status END,
                    replied=CASE WHEN ? THEN 1 ELSE discord_messages.replied END,
                    outage_response=CASE WHEN ? THEN 0 ELSE discord_messages.outage_response END,
                    response_message_id=COALESCE(?, response_message_id),
                    updated_at=?
                `, replyTo, status, flag, result.MessageID, now, flag, flag, flag, result.MessageID, now)
		return nil, err
	}, nil)
	if !completed {
		return
	}
	channelID, _ := a.withDiscordRecoveryDB(ctx, func(conn state.SQLiteConn) (any, error) {
		var v sql.NullString
		err := conn.QueryRowContext(ctx, "SELECT COALESCE(thread_id, channel_id) FROM discord_messages WHERE message_id=?", replyTo).Scan(&v)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && (!v.Valid || v.String == "")) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return v.String, nil
	}, nil).(string)
	if channelID != "" {
		a.advanceDiscordRecoveryCursor(ctx, channelID, replyTo)
	}
}

// Whether the message is recorded as answered by a real response
func (a *DiscordAdapter) discordMessageIsPersistentlyComplete(ctx context.Context, messageID string) bool {
	if messageID == "" {
		return false
	}
	v := a.withDiscordRecoveryDB(ctx, func(conn state.SQLiteConn) (any, error) {
		var status sql.NullString
		var replied, outage sql.NullInt64
		err := conn.QueryRowContext(ctx, "SELECT status, replied, outage_response FROM discord_messages WHERE message_id=?", messageID).Scan(&status, &replied, &outage)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return nil, err
		}
		return status.String == "responded" && replied.Int64 != 0 && outage.Int64 == 0, nil
	}, false)
	return v == true
}

// Whether the message is queued or processing within the last ten minutes, true when the ledger fails
func (a *DiscordAdapter) discordMessageHasActiveClaim(ctx context.Context, messageID string) bool {
	if messageID == "" {
		return false
	}
	cutoff := isoformat(time.Now().UTC().Add(-10 * time.Minute))
	v := a.withDiscordRecoveryDB(ctx, func(conn state.SQLiteConn) (any, error) {
		var status, updated sql.NullString
		err := conn.QueryRowContext(ctx, "SELECT status, updated_at FROM discord_messages WHERE message_id=?", messageID).Scan(&status, &updated)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return nil, err
		}
		return (status.String == "queued" || status.String == "processing") && updated.String >= cutoff, nil
	}, true)
	return v == true
}

// Whether the message was already dispatched the maximum number of times
func (a *DiscordAdapter) discordMessageAttemptsExhausted(ctx context.Context, messageID string) bool {
	if messageID == "" {
		return false
	}
	maxAttempts := a.missedMessageBackfillMaxAttempts(ctx)
	v := a.withDiscordRecoveryDB(ctx, func(conn state.SQLiteConn) (any, error) {
		var attempts sql.NullInt64
		err := conn.QueryRowContext(ctx, "SELECT attempts FROM discord_messages WHERE message_id=?", messageID).Scan(&attempts)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return nil, err
		}
		return int(attempts.Int64) >= maxAttempts, nil
	}, false)
	exhausted := v == true
	if exhausted {
		pkgLog().Debug(fmt.Sprintf("[%s] Not re-dispatching Discord message %s: missed_message_backfill.max_attempts (%d) reached", a.Name(), messageID, maxAttempts))
	}
	return exhausted
}

// Inserts a running scan row and returns its id
func (a *DiscordAdapter) recordRecoveryScanStart(ctx context.Context, channels strSet) string {
	scanID := fmt.Sprintf("%d-%d", time.Now().UnixMilli(), os.Getpid())
	now := utcNowISO()
	sorted := keysOf(channels)
	sort.Strings(sorted)
	channelsJSON, _ := jsonutil.Encode(sorted, jsonutil.Spaced(), jsonutil.ASCII())
	a.withDiscordRecoveryDB(ctx, func(conn state.SQLiteConn) (any, error) {
		_, err := conn.ExecContext(ctx, "INSERT OR REPLACE INTO discord_recovery_scans (scan_id, started_at, status, channels, window_seconds, limit_count) VALUES (?, ?, ?, ?, ?, ?)",
			scanID, now, "running", channelsJSON, a.missedMessageBackfillWindowSeconds(ctx), a.missedMessageBackfillLimit(ctx))
		return nil, err
	}, nil)
	return scanID
}

// Closes a scan row with its outcome counts
func (a *DiscordAdapter) recordRecoveryScanComplete(ctx context.Context, scanID, status string, scanned, missed, dispatched int, errText *string) {
	now := utcNowISO()
	a.withDiscordRecoveryDB(ctx, func(conn state.SQLiteConn) (any, error) {
		_, err := conn.ExecContext(ctx, "UPDATE discord_recovery_scans SET completed_at=?, status=?, scanned=?, missed=?, dispatched=?, error=? WHERE scan_id=?",
			now, status, scanned, missed, dispatched, errText, scanID)
		return nil, err
	}, nil)
}

// Last message id the recovery scan reached in a channel
func (a *DiscordAdapter) discordRecoveryCursor(ctx context.Context, channelID string) string {
	if channelID == "" {
		return ""
	}
	v, _ := a.withDiscordRecoveryDB(ctx, func(conn state.SQLiteConn) (any, error) {
		var id sql.NullString
		err := conn.QueryRowContext(ctx, "SELECT last_message_id FROM discord_recovery_cursors WHERE channel_id=?", channelID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return id.String, nil
	}, nil).(string)
	return v
}

// Moves a channel's recovery cursor to the given message id
func (a *DiscordAdapter) advanceDiscordRecoveryCursor(ctx context.Context, channelID, messageID string) {
	if channelID == "" || messageID == "" {
		return
	}
	now := utcNowISO()
	a.withDiscordRecoveryDB(ctx, func(conn state.SQLiteConn) (any, error) {
		_, err := conn.ExecContext(ctx, `
                INSERT INTO discord_recovery_cursors (channel_id, last_message_id, updated_at)
                VALUES (?, ?, ?)
                ON CONFLICT(channel_id) DO UPDATE SET
                    last_message_id=excluded.last_message_id,
                    updated_at=excluded.updated_at
                `, channelID, messageID, now)
		return nil, err
	}, nil)
}

// Whether the text is an explicit outage notice of the agent or gateway
func isDownNoticeContent(content string) bool {
	return downNoticeRE.MatchString(strings.ToLower(content))
}
