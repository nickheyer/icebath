package bluebubbles

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

func TestSendAnimationGoesThroughTheAdaptersOwnImageSend(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	saved := cacheImageFromURL
	t.Cleanup(func() { cacheImageFromURL = saved })
	var fetched []string
	cacheImageFromURL = func(ctx context.Context, url, ext string, retries int) (string, error) {
		fetched = append(fetched, url+" "+ext)
		return t.TempDir() + "/missing.gif", nil
	}
	adapter, err := NewBlueBubblesAdapter(context.Background(), &gateway.PlatformConfig{Extra: map[string]any{
		"server_url": "http://127.0.0.1:1", "password": "local-test"}})
	if err != nil {
		t.Fatal(err)
	}
	var surface gateway.PlatformAdapter = adapter
	result, err := surface.SendAnimation(context.Background(), "chat-1", "https://example.invalid/wave.gif", "hello", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(fetched) != 1 || fetched[0] != "https://example.invalid/wave.gif .jpg" {
		t.Fatalf("the animation was not cached as an image attachment, fetches %q", fetched)
	}
	if result.Success || result.Error != "Not connected" {
		t.Fatalf("an adapter that never connected answered %#v", result)
	}
}
