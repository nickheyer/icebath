package discord

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// One captured standalone POST, its path, JSON body and multipart payload_json
type standaloneCall struct {
	path        string
	json        map[string]any
	payloadJSON map[string]any
	isMultipart bool
}

// Loopback Discord API answering each POST with the next id
func standaloneAPI(t *testing.T, ids ...string) *[]standaloneCall {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	var mu sync.Mutex
	calls := &[]standaloneCall{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := standaloneCall{path: r.URL.Path}
		mediaType, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if strings.HasPrefix(mediaType, "multipart/") {
			c.isMultipart = true
			mr := multipart.NewReader(r.Body, params["boundary"])
			for {
				part, err := mr.NextPart()
				if err != nil {
					break
				}
				if part.FormName() == "payload_json" {
					raw, _ := io.ReadAll(part)
					json.Unmarshal(raw, &c.payloadJSON)
				}
			}
		} else {
			json.NewDecoder(r.Body).Decode(&c.json)
		}
		mu.Lock()
		id := ids[min(len(*calls), len(ids)-1)]
		*calls = append(*calls, c)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"`+id+`"}`)
	}))
	t.Cleanup(srv.Close)
	prev := discordStandaloneAPIBase
	discordStandaloneAPIBase = srv.URL + "/api/v10"
	t.Cleanup(func() { discordStandaloneAPIBase = prev })
	return calls
}

func tmpMedia(t *testing.T, name string) string {
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func standalonePconfig() *gateway.PlatformConfig {
	cfg := gateway.NewPlatformConfig()
	cfg.Token = "bot-token"
	return cfg
}

func TestCaptionRidesMediaNonForum(t *testing.T) {
	calls := standaloneAPI(t, "m1")
	chatID := "999000111"
	rememberChannelIsForum(chatID, false)
	res, err := standaloneSend(context.Background(), map[string]any{"pconfig": standalonePconfig(), "chat_id": chatID, "message": "",
		"media_files": []gateway.MediaRef{{Path: tmpMedia(t, "a.png")}}, "caption": "2-bedroom floor plan"})
	if err != nil || res["success"] != true {
		t.Fatalf("res %v err %v", res, err)
	}
	if len(*calls) != 1 || !strings.HasSuffix((*calls)[0].path, "/messages") || (*calls)[0].payloadJSON["content"] != "2-bedroom floor plan" {
		t.Fatalf("calls %+v", *calls)
	}
}

func TestNoCaptionNonForumKeepsSeparateText(t *testing.T) {
	calls := standaloneAPI(t, "t1", "m1")
	chatID := "999000222"
	rememberChannelIsForum(chatID, false)
	res, err := standaloneSend(context.Background(), map[string]any{"pconfig": standalonePconfig(), "chat_id": chatID, "message": "hello",
		"media_files": []gateway.MediaRef{{Path: tmpMedia(t, "a.png")}}})
	if err != nil || res["success"] != true {
		t.Fatalf("res %v err %v", res, err)
	}
	if len(*calls) != 2 || (*calls)[0].json["content"] != "hello" || len((*calls)[0].json) != 1 || !strings.HasSuffix((*calls)[1].path, "/messages") {
		t.Fatalf("calls %+v", *calls)
	}
}
