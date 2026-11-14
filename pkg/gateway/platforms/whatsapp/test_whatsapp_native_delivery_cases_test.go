package whatsapp

import (
	"context"
	"testing"
)

func TestWhatsAppNativeFormattingInvisibleUnicodePrefixesAreSanitized(t *testing.T) {
	if got := openAdapter(t).FormatMessage("⁠ text"); got != " text" {
		t.Fatalf("got %q", got)
	}
}

func TestSendLocationPostsToBridgeLocationEndpoint(t *testing.T) {
	a := openAdapter(t)
	posts := recordingBridge(t, a, "loc-msg")
	result := a.SendLocation(context.Background(), "15551234567", 41.015, 28.979, "HQ", "Example Street")
	if !result.Success || result.MessageID != "loc-msg" || len(*posts) != 1 {
		t.Fatalf("result = %+v posts = %v", result, *posts)
	}
	p := (*posts)[0]
	if p["_path"] != "/send-location" || p["chatId"] != "15551234567@s.whatsapp.net" || p["latitude"] != 41.015 ||
		p["longitude"] != 28.979 || p["name"] != "HQ" || p["address"] != "Example Street" || len(p) != 6 {
		t.Fatalf("payload = %v", p)
	}
}
