package gateway

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Suffix of a store outage copy taken for re-dispatch, outside every replay glob until its turn settles
const storeOutageTakenSuffix = ".redispatch"

// One store outage copy turned back into an inbound message, with the file that keeps it until its turn settles
type StoreOutageMessage struct {
	Event *MessageEvent
	Path  string
}

// Store outage copies of the spool rebuilt as inbound messages of their chats in arrival order, copies the store kept deleted
func TakeStoreOutageMessages(ctx context.Context, store *SessionStore) []StoreOutageMessage {
	flushDir, err := shutdownFlushDir(ctx)
	var found, taken []string
	if err == nil {
		found, err = spoolDirMatches(flushDir, "*.json")
	}
	if err == nil {
		taken, err = spoolDirMatches(flushDir, "*.json"+storeOutageTakenSuffix)
	}
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("Cannot scan the spool for store outage messages: %v", err))
		return nil
	}
	db, _ := store.DB(ctx)
	appender, _ := db.(TranscriptAppender)
	var out []StoreOutageMessage
	for _, path := range orderFlushFiles(append(taken, found...)) {
		message, ok := storeOutageMessage(ctx, store, appender, path)
		if !ok {
			continue
		}
		target := path
		if !strings.HasSuffix(path, storeOutageTakenSuffix) {
			target = path + storeOutageTakenSuffix
			if err := os.Rename(path, target); err != nil {
				pkgLog().Warn(fmt.Sprintf("Store outage message %s stays for row recovery, it could not be taken: %v", path, err))
				continue
			}
		}
		out = append(out, StoreOutageMessage{Event: message, Path: target})
	}
	if len(out) > 0 {
		pkgLog().Info(fmt.Sprintf("Re-dispatching %d message(s) a session store outage held", len(out)))
	}
	return out
}

// Inbound message of one store outage copy, false for another payload, a copy the store kept or a route with no known chat
func storeOutageMessage(ctx context.Context, store *SessionStore, appender TranscriptAppender, path string) (*MessageEvent, bool) {
	parsed, err := readSpoolPayload(path)
	payload, isObject := parsed.(*pyjson.Object)
	if err != nil || !isObject {
		return nil, false
	}
	if reason, _ := payload.Get("reason"); reason != StoreOutageReason {
		return nil, false
	}
	if appender != nil {
		if kept, err := admissionKept(ctx, appender, payload); err == nil && kept {
			removeReplayed(path, pyStrOf(payload.GetOr("session_key", "")))
			return nil, false
		}
	}
	data, _ := payload.Get("data")
	obj, isData := data.(*pyjson.Object)
	if !isData {
		return nil, false
	}
	text := pyStrOf(obj.GetOr("text", ""))
	key := pyStrOf(payload.GetOr("session_key", ""))
	entry := store.LookupBySessionKey(ctx, key)
	if text == "" || entry == nil || entry.Origin == nil {
		return nil, false
	}
	source := *entry.Origin
	return &MessageEvent{Text: text, MessageType: MessageTypeText, Source: &source}, true
}

// Deletes the copy of a re-dispatched message once its turn settled
func SettleStoreOutageMessage(path string) {
	removeReplayed(path, path)
}
