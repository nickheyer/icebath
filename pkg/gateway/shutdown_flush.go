package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const (
	TranscriptCapDropReason = "transcript_cap_drop"
	AgentHistoryReason      = "shutdown-with-unpersisted-agent-history"
	QuarantineSuffix        = ".bad"
)

var (
	shutdownFlushDir = defaultShutdownFlushDir
	shutdownFlushNow = ledgerNow
	flushUUIDHex     = func() string { return strings.ReplaceAll(uuid.NewString(), "-", "") }
)

// Private pending_messages directory under the active home, refused for a deleted or missing named profile
func defaultShutdownFlushDir(ctx context.Context) (string, error) {
	flushDir := pypath.PathJoin(home.Dir(ctx), "pending_messages")
	if err := home.AssertProfileLive(flushDir); err != nil {
		return "", err
	}
	if _, err := home.MakeUserDir(ctx, flushDir, 0o700, true, true); err != nil {
		return "", err
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(flushDir, 0o700); err != nil {
			return "", err
		}
	}
	return flushDir, nil
}

// Atomically writes one private uniquely named recovery payload and fsyncs its directory entry
func writeFlushPayload(ctx context.Context, flushDir string, payload *pyjson.Object) (string, error) {
	finalPath := pypath.PathJoin(flushDir, "pending-"+flushUUIDHex()+".json")
	if err := config.AtomicJSONWriteMode(ctx, finalPath, payload, 0o600, pyjson.Indent(2),
		pyjson.Default(func(v any) (any, error) { return pyrepr.Str(v), nil })); err != nil {
		return "", err
	}
	if runtime.GOOS != "windows" {
		dir, err := os.Open(flushDir)
		if err != nil {
			slog.Debug(fmt.Sprintf("Failed to fsync pending-message directory: %v", err))
			return finalPath, nil
		}
		if err := dir.Sync(); err != nil {
			slog.Debug(fmt.Sprintf("Failed to fsync pending-message directory: %v", err))
		}
		dir.Close()
	}
	return finalPath, nil
}

// One extra payload field in the order it is written after session_key
type flushField struct {
	key   string
	value any
}

// Serialises and writes one pending value, false when nothing was written
func flushValue(ctx context.Context, flushDir, kind, sessionKey string, value any, extra ...flushField) bool {
	serialised, ok := serialiseFlushValue(value)
	if !ok {
		return false
	}
	payload := pyjson.NewObject()
	payload.Set("session_key", sessionKey)
	for _, f := range extra {
		payload.Set(f.key, f.value)
	}
	payload.Set("data", serialised)
	if _, err := writeFlushPayload(ctx, flushDir, payload); err != nil {
		slog.Debug(fmt.Sprintf("Failed to flush %s message for %s: %v", kind, sessionKey, err))
		return false
	}
	return true
}

// Serialises the non empty pending slots of an adapter and returns how many were written
func FlushPendingToFile(ctx context.Context, pending map[string]*MessageEvent, reason string) (int, error) {
	values := make(map[string]any, len(pending))
	for k, v := range pending {
		if v != nil {
			values[k] = v
		} else {
			values[k] = nil
		}
	}
	return FlushPendingValuesToFile(ctx, values, reason)
}

// Serialises non empty pending slots holding events, runner text or mappings and returns the count
func FlushPendingValuesToFile(ctx context.Context, pending map[string]any, reason string) (int, error) {
	if len(pending) == 0 {
		return 0, nil
	}
	flushDir, err := shutdownFlushDir(ctx)
	if err != nil {
		return 0, err
	}
	ts := int64(shutdownFlushNow())
	flushed := 0
	for sessionKey, value := range pending {
		if value == nil {
			continue
		}
		if flushValue(ctx, flushDir, "pending", sessionKey, value, flushField{"reason", reason}, flushField{"ts", ts}) {
			flushed++
		}
	}
	if flushed > 0 {
		slog.Info(fmt.Sprintf("Flushed %d pending message(s) to %s (reason=%s)", flushed, flushDir, reason))
	}
	return flushed, nil
}

// Serialises the FIFO overflow tails one payload per event with its arrival seq and returns the count
func FlushOverflowToFile(ctx context.Context, overflowBySession map[string][]any, reason string) (int, error) {
	if len(overflowBySession) == 0 {
		return 0, nil
	}
	flushDir, err := shutdownFlushDir(ctx)
	if err != nil {
		return 0, err
	}
	ts := int64(shutdownFlushNow())
	flushed := 0
	for sessionKey, events := range overflowBySession {
		if sessionKey == "" || len(events) == 0 {
			continue
		}
		for seq, value := range append([]any(nil), events...) {
			if value == nil {
				continue
			}
			if flushValue(ctx, flushDir, "overflow", sessionKey, value, flushField{"reason", reason}, flushField{"ts", ts},
				flushField{"seq", seq}) {
				flushed++
			}
		}
	}
	if flushed > 0 {
		slog.Info(fmt.Sprintf("Flushed %d queued overflow message(s) to %s (reason=%s)", flushed, flushDir, reason))
	}
	return flushed, nil
}

// Whether json.dumps accepts the value
func flushJSONSafe(value any) bool {
	_, err := pyjson.Dumps(value)
	return err == nil
}

// Attribute lookup of a message like pending value, false for an attribute the value lacks
type pendingAttrValue interface {
	pendingAttr(name string) (any, bool)
}

// Text attribute of an event, the only flushed attribute a MessageEvent carries
func (e *MessageEvent) pendingAttr(name string) (any, bool) {
	if name == "text" {
		return e.Text, true
	}
	return nil, false
}

var flushedEventAttrs = []string{"session_id", "platform", "sender_id", "sender_name", "reply_to", "media", "raw_event"}

// JSON serialisable mapping of a pending value, false when there is nothing to write
func serialiseFlushValue(value any) (any, bool) {
	if v, ok := value.(pendingAttrValue); ok {
		if text, has := v.pendingAttr("text"); has {
			result := pyjson.NewObject()
			result.Set("text", text)
			for _, attr := range flushedEventAttrs {
				val, present := v.pendingAttr(attr)
				if !present || val == nil {
					continue
				}
				if flushJSONSafe(val) {
					result.Set(attr, val)
				} else {
					result.Set(attr, pyrepr.Str(val))
				}
			}
			return result, true
		}
	}
	if s, ok := value.(string); ok {
		result := pyjson.NewObject()
		result.Set("text", s)
		return result, true
	}
	switch value.(type) {
	case map[string]any, *pyjson.Object:
		if flushJSONSafe(value) {
			return value, true
		}
	}
	result := pyjson.NewObject()
	result.Set("text", pyrepr.Str(value))
	return result, true
}
