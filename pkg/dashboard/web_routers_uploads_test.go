package dashboard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pytempfile"
)

// Multipart body of text fields and files the way httpx encodes them, with its content type
func multipartBody(boundary string, fields [][2]string, files [][4]string) ([]byte, string) {
	var b bytes.Buffer
	for _, f := range fields {
		fmt.Fprintf(&b, "--%s\r\nContent-Disposition: form-data; name=\"%s\"\r\n\r\n%s\r\n", boundary, f[0], f[1])
	}
	for _, f := range files {
		fmt.Fprintf(&b, "--%s\r\nContent-Disposition: form-data; name=\"%s\"; filename=\"%s\"\r\nContent-Type: %s\r\n\r\n%s\r\n",
			boundary, f[0], f[1], f[2], f[3])
	}
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return b.Bytes(), "multipart/form-data; boundary=" + boundary
}

func TestBrowserUploadChecksActualSpooledSizeAndClosesUpload(t *testing.T) {
	for _, payload := range [][]byte{{}, []byte("12345678"), []byte("123456789")} {
		t.Run(fmt.Sprintf("len%d", len(payload)), func(t *testing.T) {
			h := filepath.Join(t.TempDir(), "icebath-home")
			if err := os.Mkdir(h, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("ICEBATH_HOME", h)
			saved := uploadsMaxUploadBytes
			uploadsMaxUploadBytes = 8
			t.Cleanup(func() { uploadsMaxUploadBytes = saved })
			spool := pytempfile.NewSpooledTemporaryFile(4)
			if _, err := spool.Write(payload); err != nil {
				t.Fatal(err)
			}
			name, size := "notes.txt", int64(0)
			file := &UploadFile{Filename: &name, File: spool, Size: &size, Headers: HeadersOf()}
			result, err := uploadChatFile(context.Background(), file, "")
			if len(payload) > 8 {
				var h413 *HTTPException
				if !errors.As(err, &h413) || h413.StatusCode != 413 {
					t.Fatalf("want 413, got %v", err)
				}
				if _, err := os.Stat(filepath.Join(h, "uploads")); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("uploads dir created: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if got, _ := result.Vals["size"].(int64); got != int64(len(payload)) {
					t.Fatalf("size %v", result.Vals["size"])
				}
				data, err := os.ReadFile(result.Vals["path"].(string))
				if err != nil || !bytes.Equal(data, payload) {
					t.Fatalf("staged %q %v", data, err)
				}
			}
			if !spool.Closed() {
				t.Fatal("spool left open")
			}
		})
	}
}

func TestBrowserFileCleanupFailurePreservesPrimaryStagingError(t *testing.T) {
	h := filepath.Join(t.TempDir(), "icebath-home")
	target := filepath.Join(h, "uploads", "web-target")
	if err := os.MkdirAll(filepath.Join(target, "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}
	opened := &fs.PathError{Op: "open", Path: target, Err: errors.New("target open denied")}
	err := stageFailure(removedOnFailure(target, func() error { return opened }))
	var h500 *HTTPException
	if !errors.As(err, &h500) || h500.StatusCode != 500 || !strings.Contains(h500.Detail.(string), "target open denied") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("cleanup target vanished: %v", err)
	}
}

func TestMultipartUploadRouteStagesTheFileAndValidatesTheFileField(t *testing.T) {
	h := filepath.Join(t.TempDir(), "icebath-home")
	if err := os.Mkdir(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	app := NewFastAPI("t", "1", nil, nil)
	app.IncludeRouter(uploadsRouter(), "")
	client := newTestClient(t, app)

	body, ct := multipartBody("b0a1c2", nil, [][4]string{{"file", "../notes from browser.txt", "text/plain", "browser bytes"}})
	resp := client.request("POST", "/api/chat/file-upload", body, "content-type", ct)
	if resp.Status != 200 {
		t.Fatalf("status %d %s %v", resp.Status, resp.Body, resp.AppErr)
	}
	payload := resp.JSON(t).(*pyjson.Object)
	staged := payload.Vals["path"].(string)
	if filepath.Dir(staged) != filepath.Join(h, "uploads") || !strings.HasSuffix(staged, "notes-from-browser.txt") {
		t.Fatalf("staged at %s", staged)
	}
	if data, _ := os.ReadFile(staged); string(data) != "browser bytes" {
		t.Fatalf("staged bytes %q", data)
	}

	body, ct = multipartBody("b0a1c2", [][2]string{{"file", "plain text"}}, nil)
	resp = client.request("POST", "/api/chat/file-upload", body, "content-type", ct)
	resp.expect(t, 422, `{"detail":[{"type":"value_error","loc":["body","file"],"msg":"Value error, Expected UploadFile, received: <class 'str'>","input":"plain text","ctx":{"error":{}}}]}`)

	resp = client.request("POST", "/api/chat/file-upload", []byte("a=b"), "content-type", "application/x-www-form-urlencoded")
	resp.expect(t, 422, `{"detail":[{"type":"missing","loc":["body","file"],"msg":"Field required","input":null}]}`)

	resp = client.request("POST", "/api/chat/file-upload", []byte("--x\r\n"), "content-type", "multipart/form-data")
	resp.expect(t, 400, `{"detail":"Missing boundary in multipart."}`)

	resp = client.request("POST", "/api/chat/file-upload", []byte("garbage"), "content-type", "multipart/form-data; boundary=zz")
	resp.expect(t, 400, `{"detail":"Invalid multipart data."}`)
}
