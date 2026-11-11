package gateway

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/mediacache"
	"github.com/nickheyer/icebath/pkg/state"
)

var capturePNG = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 8)...)

// Stages one image of the given size in the profile image cache
func stagedImage(t *testing.T, ctx context.Context, name string, size int) string {
	t.Helper()
	dir, err := mediacache.ImageCacheDir(ctx, home.Dir(ctx))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data := append([]byte(nil), capturePNG...)
	if size > len(data) {
		data = append(data, make([]byte, size-len(data))...)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Names of the entries directly under a directory
func dirEntryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestMissingStagingSiblingDoesNotSkipRemainingImageHashes(t *testing.T) {
	for _, missing := range []int{0, 1} {
		t.Run([]string{"first", "second"}[missing], func(t *testing.T) {
			ctx, _ := newTestStateDB(t)
			staging, err := mediacache.ImageCacheDir(ctx, home.Dir(ctx))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(staging, 0o700); err != nil {
				t.Fatal(err)
			}
			paths := []string{filepath.Join(staging, "first.png"), filepath.Join(staging, "second.png")}
			original := [][]byte{[]byte("\x89PNG\r\n\x1a\n0"), []byte("\x89PNG\r\n\x1a\n1")}
			attachments := []any{}
			for i, path := range paths {
				if err := os.WriteFile(path, original[i], 0o600); err != nil {
					t.Fatal(err)
				}
				attachments = append(attachments, map[string]any{"path": path, "mime": "image/png"})
			}
			committed, err := admitAttachments(ctx, attachments, nil)
			if err != nil {
				t.Fatal(err)
			}
			row := pyjson.ObjectOf("status", "queued", "payload", committed)
			admitted := func() (*pyjson.Object, error) { return row, nil }
			if err := os.Remove(paths[missing]); err != nil {
				t.Fatal(err)
			}
			remaining := paths[1-missing]
			if err := os.WriteFile(remaining, []byte("\x89PNG\r\n\x1a\nchanged"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := admitAttachments(ctx, attachments, admitted); !isRuntimeRefusal(err, "admission_conflict") {
				t.Fatalf("changed sibling: %v", err)
			}
			if err := os.WriteFile(remaining, original[1-missing], 0o600); err != nil {
				t.Fatal(err)
			}
			again, err := admitAttachments(ctx, attachments, admitted)
			if err != nil {
				t.Fatal(err)
			}
			if !pyjson.Equal(again, committed) {
				t.Fatalf("retry payload %v, want %v", again, committed)
			}
		})
	}
}

func TestRejectedBatchPublishesNothing(t *testing.T) {
	ctx, _ := newTestStateDB(t)
	if err := os.WriteFile(filepath.Join(home.Dir(ctx), "config.yaml"), []byte("gateway:\n  max_inbound_media_bytes: 3072\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := captureNativeMedia(ctx, []string{stagedImage(t, ctx, "owned.png", 512)})
	if err != nil {
		t.Fatal(err)
	}
	owned := rowString(first[0].(*pyjson.Object), "path")
	_, err = captureNativeMedia(ctx, []string{stagedImage(t, ctx, "owned.png", 512), stagedImage(t, ctx, "a.png", 2*1024), stagedImage(t, ctx, "b.png", 2*1024)})
	if !isRuntimeRefusal(err, "invalid_params") {
		t.Fatalf("over budget batch: %v", err)
	}
	if !onDisk(owned) {
		t.Fatal("the earlier capture's file was rolled back with the rejected batch")
	}
	root, err := nativeInputsRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if names := dirEntryNames(t, root); !slices.Equal(names, []string{filepath.Base(filepath.Dir(owned))}) {
		t.Fatalf("retained store holds %v", names)
	}
}

func TestEqualBytesUnderDifferentBasenamesAreFullyReleased(t *testing.T) {
	ctx, db := newTestStateDB(t)
	if err := db.CreateSession(state.NewSession{ID: "s", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	epoch, err := db.BeginRuntimeEpoch(ctx, "current")
	if err != nil {
		t.Fatal(err)
	}
	var rows []*pyjson.Object
	for _, name := range []string{"a.png", "b.png"} {
		media, err := captureNativeMedia(ctx, []string{stagedImage(t, ctx, name, 64)})
		if err != nil {
			t.Fatal(err)
		}
		payload := pyjson.ObjectOf("text", name, "attachments_v1", pyjson.ObjectOf("media", media, "media_types", []any{"image/png"}))
		row, err := db.AdmitSessionInput(ctx, state.SessionInput{Epoch: epoch, PrincipalID: "p", SessionID: "s", RequestID: name, Payload: payload})
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	root, err := nativeInputsRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	firstMedia := nativeAttachmentMedia(t, rows[0])
	digestDir := filepath.Join(root, rowString(firstMedia[0].(*pyjson.Object), "sha256"))
	if names := dirEntryNames(t, digestDir); !slices.Equal(names, []string{"a.png", "b.png"}) {
		t.Fatalf("digest directory holds %v", names)
	}
	if _, err := db.CancelSessionInput(ctx, epoch, rowString(rows[0], "admission_id")); err != nil {
		t.Fatal(err)
	}
	if err := releaseAdmissionMedia(ctx, db, rowString(rows[0], "admission_id")); err != nil {
		t.Fatal(err)
	}
	if !onDisk(filepath.Join(digestDir, "b.png")) {
		t.Fatal("bytes a live admission still needs must survive")
	}
	if _, err := db.CancelSessionInput(ctx, epoch, rowString(rows[1], "admission_id")); err != nil {
		t.Fatal(err)
	}
	if err := releaseAdmissionMedia(ctx, db, rowString(rows[1], "admission_id")); err != nil {
		t.Fatal(err)
	}
	if onDisk(digestDir) {
		t.Fatalf("digest directory still holds %v", dirEntryNames(t, digestDir))
	}
	if names := dirEntryNames(t, root); len(names) != 0 {
		t.Fatalf("retained store still holds %v", names)
	}
}

// Committed attachment media references of an admission row
func nativeAttachmentMedia(t *testing.T, row *pyjson.Object) []any {
	t.Helper()
	value, _ := rowPayload(row).Get("attachments_v1")
	attachments, ok := value.(*pyjson.Object)
	if !ok {
		t.Fatalf("row carries no attachments: %v", row)
	}
	media, _ := attachments.Get("media")
	list, _ := media.([]any)
	return list
}
