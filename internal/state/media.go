package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/home"
)

// Meta key prefixes of the durable retirement candidates, retained media and spillover archives
const (
	retiredMediaPrefix     = "gateway.media.retired.v1."
	retiredSpilloverPrefix = "gateway.spillover.retired.v1."
)

// Rows a spillover reference scan reads before the reference set counts as unknown
const spilloverScanLimit = 20000

// Filter selecting message rows that may point at a spillover archive
const spilloverSQLFilter = "(instr(content, 'spillover') > 0 OR instr(api_content, 'spillover') > 0)"

// Basename of a spillover archive a transcript text points at
var spilloverName = regexp.MustCompile(`spillover(?:/|\\+)([A-Za-z0-9_.-]+)`)

// Error of a media step on a store opened without the gateway's media policy
var ErrNoMediaPolicy = errors.New("store was opened without a media policy")

// Gateway media policy the store calls when it retires and collects retained media
type MediaHooks struct {
	// Retained document references the prompt of a hosted admission names, each one JSON object with a path
	HostedDocuments func(requestID string, payload json.RawMessage) ([]json.RawMessage, error)
	// Deletes the retained references no admission or retained transcript row owns
	ReleaseUnheld func(ctx context.Context, s *Store, references []json.RawMessage) error
}

// Spillover archive basenames the texts reference
func spilloverNamesInText(texts ...string) []string {
	var names []string
	for _, text := range texts {
		if !strings.Contains(text, "spillover") {
			continue
		}
		for _, m := range spilloverName.FindAllStringSubmatch(text, -1) {
			if !containsString(names, m[1]) {
				names = append(names, m[1])
			}
		}
	}
	return names
}

// Media list of one member of an admission payload
func payloadMedia(payload []configMember, key string) []json.RawMessage {
	raw, _ := memberValue(payload, key)
	section, _ := objectMembers(raw)
	media, _ := memberValue(section, "media")
	elements, _ := arrayElements(media)
	return elements
}

// Session options of the store whose layer set up the connection c runs on, false for any other connection
func sessionOptionsOf(c Conn) (SessionOptions, bool) {
	r, lent := c.(*runner)
	if !lent || r.c.sessions == nil {
		return SessionOptions{}, false
	}
	return r.c.sessions.options, true
}

// Records the media an admission payload references as durable deletion candidates
func retireMedia(ctx context.Context, c Conn, payloadJSON, requestID string) error {
	payload, isObject := objectMembers(json.RawMessage(payloadJSON))
	if !isObject {
		return runtimeErr(ReasonStorageUnavailable)
	}
	references := append(payloadMedia(payload, "attachments_v1"), payloadMedia(payload, "native_text_v1")...)
	if text, ok := memberText(payload, "text"); ok && strings.HasPrefix(requestID, "hosted:") && text != "" {
		options, _ := sessionOptionsOf(c)
		hosted := options.Media.HostedDocuments
		if hosted == nil {
			return ErrNoMediaPolicy
		}
		documents, err := hosted(requestID, json.RawMessage(payloadJSON))
		if err != nil {
			return err
		}
		references = append(references, documents...)
	}
	references = append(references, payloadMedia(payload, "api_turn_v1")...)
	return retireMediaReferences(ctx, c, references)
}

// Records retained media the gateway captured for an input that was never admitted as deletion candidates
func (s *Store) RetireMedia(ctx context.Context, references []json.RawMessage) error {
	return s.Write(ctx, WritePatience, func(c Conn) error { return retireMediaReferences(ctx, c, references) })
}

// Stores each media reference under the digest of its path, an existing candidate staying as written
func retireMediaReferences(ctx context.Context, c Conn, references []json.RawMessage) error {
	for _, ref := range references {
		members, _ := objectMembers(ref)
		path, ok := memberText(members, "path")
		if !ok {
			return runtimeErr(ReasonStorageUnavailable)
		}
		sum := sha256.Sum256([]byte(path))
		value, err := spacedJSON(ref)
		if err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, "INSERT OR IGNORE INTO state_meta(key,value) VALUES(?,?)", retiredMediaPrefix+hex.EncodeToString(sum[:]), value); err != nil {
			return err
		}
	}
	return nil
}

// Records the spillover archives the doomed sessions' messages point at, inside the delete transaction
func retireSpillover(ctx context.Context, c Conn, sessionIDs []string) error {
	for _, chunk := range chunks(sessionIDs, sqlInChunk) {
		rows, err := c.QueryContext(ctx, "SELECT content, api_content FROM messages WHERE session_id IN ("+bindMarks(len(chunk))+")"+
			" AND "+spilloverSQLFilter, stringArgs(chunk)...)
		if err != nil {
			return err
		}
		var names []string
		for rows.Next() {
			var content, api any
			if err := rows.Scan(&content, &api); err != nil {
				rows.Close()
				return err
			}
			for _, n := range spilloverNamesInText(textCell(content), textCell(api)) {
				if !containsString(names, n) {
					names = append(names, n)
				}
			}
		}
		if err := rowsDone(rows); err != nil {
			return err
		}
		for _, n := range names {
			value, err := asciiJSONString(n)
			if err != nil {
				return err
			}
			if _, err := c.ExecContext(ctx, "INSERT OR IGNORE INTO state_meta(key,value) VALUES(?,?)", retiredSpilloverPrefix+n, value); err != nil {
				return err
			}
		}
	}
	return nil
}

// Meta entries whose keys start with a prefix, read as one primary key range
func (s *Store) metaPrefixRange(ctx context.Context, prefix string) ([]MetaEntry, error) {
	low, high, ok := PrefixRange(prefix)
	if !ok {
		return nil, nil
	}
	var out []MetaEntry
	err := s.Read(ctx, func(c Conn) error {
		out = nil
		rows, err := c.QueryContext(ctx, "SELECT key,value FROM state_meta WHERE key>=? AND key<?", low, high)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e MetaEntry
			if err := rows.Scan(&e.Key, lenient{&e.Value}); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// Spillover basenames a state database beside the cache still references, false when the reference set is unknown
func (s *Store) referencedSpilloverNames(ctx context.Context) (map[string]bool, bool) {
	dbPath := filepath.Join(filepath.Dir(s.path), string(home.StateDB))
	if !exists(dbPath) {
		return map[string]bool{}, true
	}
	names := map[string]bool{}
	count := 0
	scan := func(c Conn) error {
		clear(names)
		count = 0
		rows, err := c.QueryContext(ctx, "SELECT content, api_content FROM messages WHERE "+spilloverSQLFilter+" LIMIT ?", spilloverScanLimit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var content, api any
			if err := rows.Scan(&content, &api); err != nil {
				return err
			}
			count++
			for _, n := range spilloverNamesInText(textCell(content), textCell(api)) {
				names[n] = true
			}
		}
		return rows.Err()
	}
	var err error
	if resolvePath(dbPath) == resolvePath(s.path) {
		err = s.Read(ctx, scan)
	} else {
		var db *DB
		if db, err = s.eng.OpenDB(ctx, dbPath, DBOptions{ReadOnly: true, BusyTimeout: 5 * time.Second}); err == nil {
			err = errors.Join(scan(db), db.Close())
		}
	}
	if err != nil || count > spilloverScanLimit {
		return nil, false
	}
	return names, true
}

// Unlinks retired spillover archives no surviving message references and drops their candidates
func (s *Store) collectRetiredSpillover(ctx context.Context) error {
	rows, err := s.metaPrefixRange(ctx, retiredSpilloverPrefix)
	if err != nil || len(rows) == 0 {
		return err
	}
	referenced, known := s.referencedSpilloverNames(ctx)
	if !known {
		return nil
	}
	dir := home.Spillover.In(filepath.Dir(s.path))
	var done [][]any
	for _, row := range rows {
		name := jsonString(json.RawMessage(row.Value))
		if !jsonIsString(json.RawMessage(row.Value)) {
			return runtimeErr(ReasonStorageUnavailable)
		}
		if !referenced[name] {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				continue
			}
		}
		done = append(done, []any{row.Key})
	}
	if len(done) == 0 {
		return nil
	}
	return s.WriteMany(ctx, WritePatience, "DELETE FROM state_meta WHERE key=?", done)
}

// Releases retired media candidates no admission or retained transcript row owns and drops the collected keys
func (s *Store) collectRetiredMedia(ctx context.Context) error {
	if err := s.collectRetiredSpillover(ctx); err != nil {
		return err
	}
	rows, err := s.metaPrefixRange(ctx, retiredMediaPrefix)
	if err != nil || len(rows) == 0 {
		return err
	}
	release := s.tables.state.options.Media.ReleaseUnheld
	if release == nil {
		return ErrNoMediaPolicy
	}
	references := make([]json.RawMessage, len(rows))
	for i, row := range rows {
		references[i] = json.RawMessage(row.Value)
	}
	if err := release(ctx, s, references); err != nil {
		return err
	}
	var gone [][]any
	for _, row := range rows {
		members, _ := objectMembers(json.RawMessage(row.Value))
		path, ok := memberText(members, "path")
		if !ok {
			return runtimeErr(ReasonStorageUnavailable)
		}
		if !exists(path) {
			gone = append(gone, []any{row.Key})
		}
	}
	if len(gone) == 0 {
		return nil
	}
	return s.WriteMany(ctx, WritePatience, "DELETE FROM state_meta WHERE key=?", gone)
}

// Collects retired media and spillover archives after a delete or prune committed
func (s *Store) CollectRetiredMedia(ctx context.Context) error { return s.collectRetiredMedia(ctx) }
