package dashboard

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Default client transport answering every request with the response the case builds, the urlopen the hermes cases patch
func fakeVoicesTransport(t *testing.T, respond func(*http.Request) (*http.Response, error)) {
	saved := http.DefaultClient.Transport
	http.DefaultClient.Transport = roundTripFunc(respond)
	t.Cleanup(func() { http.DefaultClient.Transport = saved })
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Body counting the bytes the route consumed
type countingBody struct {
	r    io.Reader
	read atomic.Int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.read.Add(int64(n))
	return n, err
}

func (b *countingBody) Close() error { return nil }

// Endless stream of one byte
type endlessBytes struct{}

func (endlessBytes) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

// Isolated home with no .env key and the ElevenLabs key only in the process env
func voicesHome(t *testing.T, key string) {
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.Mkdir(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ELEVENLABS_API_KEY", key)
}

func TestVoicesRouteScopeFailureNeverBorrowsEnv(t *testing.T) {
	voicesHome(t, "sk-foreign-profile")
	fakeVoicesTransport(t, func(*http.Request) (*http.Response, error) {
		t.Error("network must not be reached without a key")
		return nil, errors.New("network must not be reached without a key")
	})
	ctx := provider.WithSecretScope(provider.WithMultiplexContext(context.Background(), true), map[string]string{}, os.Getenv("ICEBATH_HOME"))
	result, err := getElevenLabsVoices(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := pyjson.Dumps(result); got != `{"available": false, "voices": []}` {
		t.Fatalf("result %s", got)
	}
}

func TestVoicesRouteBoundsResponseRead(t *testing.T) {
	voicesHome(t, "sk-test")
	body := &countingBody{r: strings.NewReader(`{"voices": [{"voice_id": "voice-1", "name": "Voice One"}]}`)}
	fakeVoicesTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("xi-api-key") != "sk-test" || r.URL.String() != elevenLabsVoicesURL {
			t.Errorf("request %s %v", r.URL, r.Header)
		}
		return &http.Response{StatusCode: 200, Status: "200 OK", Body: body, Header: http.Header{}}, nil
	})
	result, err := getElevenLabsVoices(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	plain := pyjson.Plain(result).(map[string]any)
	if plain["available"] != true || plain["voices"].([]any)[0].(map[string]any)["voice_id"] != "voice-1" {
		t.Fatalf("result %v", plain)
	}
	if body.read.Load() > dashboardJSONResponseBodyMaxBytes+1 {
		t.Fatalf("read %d bytes", body.read.Load())
	}
}

func TestVoicesRouteRejectsOversizedResponse(t *testing.T) {
	voicesHome(t, "sk-test")
	body := &countingBody{r: endlessBytes{}}
	fakeVoicesTransport(t, func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Status: "200 OK", Body: body, Header: http.Header{}}, nil
	})
	_, err := getElevenLabsVoices(context.Background(), "")
	if h, ok := asHTTPException(err); !ok || h.StatusCode != 502 {
		t.Fatalf("err %v", err)
	}
	if body.read.Load() != dashboardJSONResponseBodyMaxBytes+1 {
		t.Fatalf("read %d bytes, want the cap plus one", body.read.Load())
	}
}
