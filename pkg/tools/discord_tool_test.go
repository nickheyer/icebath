package tools

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Local stand in for the Discord REST base recording the last request
func discordStandIn(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	saved := DiscordAPIBase
	DiscordAPIBase = srv.URL + "/api/v10"
	t.Cleanup(func() { DiscordAPIBase = saved })
}

func TestDiscordRequestGetRequest(t *testing.T) {
	if DiscordAPIBase != "https://discord.com/api/v10" {
		t.Fatalf("base %q", DiscordAPIBase)
	}
	var got *http.Request
	discordStandIn(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.Write([]byte(`{"ok": true}`))
	})
	result, err := DiscordRequest(context.Background(), "GET", "/test", "token123", nil, nil, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	obj, ok := result.(*pyjson.Object)
	if !ok || obj.Len() != 1 || obj.Vals["ok"] != true {
		t.Fatalf("result %#v", result)
	}
	if got.URL.Path != "/api/v10/test" || got.Header.Get("Authorization") != "Bot token123" || got.Method != "GET" {
		t.Fatalf("request %s %s %q", got.Method, got.URL.Path, got.Header.Get("Authorization"))
	}
}

func TestDiscordRequestResponseBodySizeLimit(t *testing.T) {
	saved := discordResponseBodyMaxBytes
	discordResponseBodyMaxBytes = 8
	t.Cleanup(func() { discordResponseBodyMaxBytes = saved })
	discordStandIn(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(strings.Repeat("x", 9))) })
	_, err := DiscordRequest(context.Background(), "GET", "/test", "tok", nil, nil, 15*time.Second)
	var apiErr *DiscordAPIError
	if !errors.As(err, &apiErr) || apiErr.Status != 502 || !strings.Contains(apiErr.Body, "response body exceeded 8 bytes") {
		t.Fatalf("err %v", err)
	}
}

func TestDiscordRequestRaisesForEveryAnswerOutsideTwoHundreds(t *testing.T) {
	discordStandIn(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v10/gone" {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message": "Unknown Channel"}`))
			return
		}
		w.WriteHeader(http.StatusNotModified)
	})
	_, err := DiscordRequest(context.Background(), "GET", "/cached", "tok", nil, nil, 15*time.Second)
	var apiErr *DiscordAPIError
	if !errors.As(err, &apiErr) || apiErr.Status != 304 || apiErr.Body != "" {
		t.Fatalf("304 err %v", err)
	}
	_, err = DiscordRequest(context.Background(), "GET", "/gone", "tok", nil, nil, 15*time.Second)
	if !errors.As(err, &apiErr) || apiErr.Error() != `Discord API error 404: {"message": "Unknown Channel"}` {
		t.Fatalf("404 err %v", err)
	}
}
