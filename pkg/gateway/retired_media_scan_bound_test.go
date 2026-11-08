package gateway

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestCandidatesShareOneMessagesPass(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("ICEBATH_HOME", home)
	saved, savedFilter, savedNames, savedReferenced := state.ReleaseUnheldMedia, state.SpilloverSQLFilter, state.SpilloverNamesInText, state.ReferencedSpilloverNames
	state.ReleaseUnheldMedia = ReleaseUnheldMediaRetainingHistory
	state.SpilloverSQLFilter = tools.SpilloverSQLFilter
	state.SpilloverNamesInText = tools.SpilloverNamesInText
	state.ReferencedSpilloverNames = tools.ReferencedSpilloverNames
	t.Cleanup(func() {
		state.ReleaseUnheldMedia, state.SpilloverSQLFilter, state.SpilloverNamesInText, state.ReferencedSpilloverNames = saved, savedFilter, savedNames, savedReferenced
	})
	db, err := state.NewSessionDB(ctx, filepath.Join(home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	root, err := nativeInputsRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(state.NewSession{ID: "keep", Source: "api_server"}); err != nil {
		t.Fatal(err)
	}
	var paths []string
	var references []*pyjson.Object
	for index := range 6 {
		sha := fmt.Sprintf("%064x", index)
		path := filepath.Join(root, sha, "api_"+sha[:32]+".png")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
		references = append(references, pyjson.ObjectOf("path", path, "sha256", sha, "size", int64(1)))
	}
	for _, path := range paths[:3] {
		if _, err := db.AppendMessage(ctx, "keep", map[string]any{"role": "user", "content": "[Image attached at: " + path + "]"}, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.AppendMessage(ctx, "keep", map[string]any{"role": "user", "content": "unrelated history"}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	err = db.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		for index, reference := range references {
			text, err := pyjson.Dumps(reference)
			if err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?)", state.MediaRetiredPrefix+strconv.Itoa(index), text); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var scans []string
	if err := db.SetTraceCallback(func(sql string) {
		if strings.Contains(sql, "messages") && strings.Contains(sql, "instr(") {
			mu.Lock()
			scans = append(scans, sql)
			mu.Unlock()
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.CollectRetiredMedia(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTraceCallback(nil); err != nil {
		t.Fatal(err)
	}
	full := 0
	for _, sql := range scans {
		if !strings.Contains(strings.ReplaceAll(sql, " ", ""), "id>") {
			full++
		}
	}
	if full != 1 {
		t.Fatalf("%d candidates must share one messages scan, saw %d in %q", len(references), full, scans)
	}
	for index, path := range paths {
		_, statErr := os.Stat(path)
		if exists := statErr == nil; exists != (index < 3) {
			t.Fatalf("reference %d exists %v", index, exists)
		}
	}
	var left []string
	err = db.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		rows, err := conn.QueryContext(ctx, "SELECT key FROM state_meta WHERE key>=? AND key<?", state.MediaRetiredPrefix, state.MediaRetiredPrefix[:len(state.MediaRetiredPrefix)-1]+"/")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				return err
			}
			left = append(left, key)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(left)
	want := []string{state.MediaRetiredPrefix + "0", state.MediaRetiredPrefix + "1", state.MediaRetiredPrefix + "2"}
	if strings.Join(left, ",") != strings.Join(want, ",") {
		t.Fatalf("candidates left %v", left)
	}
}
