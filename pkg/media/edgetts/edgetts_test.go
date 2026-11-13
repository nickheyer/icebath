package edgetts

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestTokenMatchesServiceVectors(t *testing.T) {
	vectors := map[int64]string{
		0:          "52BC7A17B50DFEC9-28C4394775C8751CD2F6859C030C06DB1489ACAB3B3A7651831FC26895177ED6",
		1700000000: "2CD23A2CA0B2595A-F3C846C370E05F9CC83ACC5F995C5F5D87A4D0F14CE8936A0F902E64099E38A2",
		1794500000: "02FEF51AF0719283-98A60ABEDFEE36595E1B1925B37247BEEFAB308AC0D474DAF467827D280066CF",
		1794500299: "916762F121746CB2-7F74F2EBF96AE4B10044843F4C206C5F734A86A2C98020C79CE3820934864385",
		1794500300: "916762F121746CB2-7F74F2EBF96AE4B10044843F4C206C5F734A86A2C98020C79CE3820934864385",
	}
	for at, want := range vectors {
		if got := secMSGEC(time.Unix(at, 750_000_000)); got != want {
			t.Errorf("token at %d = %s, want %s", at, got, want)
		}
	}
}

func TestVoiceSpellings(t *testing.T) {
	cases := map[string]string{
		"en-US-AriaNeural":             "Microsoft Server Speech Text to Speech Voice (en-US, AriaNeural)",
		"fil-PH-AngeloNeural":          "Microsoft Server Speech Text to Speech Voice (fil-PH, AngeloNeural)",
		"zh-CN-liaoning-XiaobeiNeural": "Microsoft Server Speech Text to Speech Voice (zh-CN-liaoning, XiaobeiNeural)",
		"Microsoft Server Speech Text to Speech Voice (cy-GB, NiaNeural)": "Microsoft Server Speech Text to Speech Voice (cy-GB, NiaNeural)",
	}
	for in, want := range cases {
		got, err := serviceVoice(in)
		if err != nil || got != want {
			t.Errorf("serviceVoice(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
	if _, err := serviceVoice("aria"); err == nil {
		t.Error("a bare word passed as a voice")
	}
}

func TestSplitKeepsRunesAndEntitiesWhole(t *testing.T) {
	text := []byte(strings.Repeat("é", 30) + " tom &amp; jerry " + strings.Repeat("x", 40))
	var joined []byte
	for _, piece := range splitText(text, 24) {
		if len(piece) > 24 {
			t.Fatalf("piece of %d bytes exceeds the limit", len(piece))
		}
		if !bytes.Equal(bytes.ToValidUTF8(piece, nil), piece) {
			t.Fatalf("piece %q cuts a rune", piece)
		}
		if at := bytes.LastIndexByte(piece, '&'); at >= 0 && bytes.IndexByte(piece[at:], ';') < 0 {
			t.Fatalf("piece %q cuts an entity", piece)
		}
		joined = append(joined, piece...)
	}
	if want := bytes.ReplaceAll(text, []byte(" "), nil); !bytes.Equal(bytes.ReplaceAll(joined, []byte(" "), nil), want) {
		t.Fatalf("pieces lost text: %q", joined)
	}
}

type fakeService struct {
	t        *testing.T
	clock    time.Time
	mu       sync.Mutex
	refused  int
	requests []string
}

func audioFrame(payload string) []byte {
	head := "X-RequestId:abc\r\nContent-Type:audio/mpeg\r\nPath:audio\r\n"
	frame := binary.BigEndian.AppendUint16(nil, uint16(len(head)))
	return append(append(frame, head...), payload...)
}

func (s *fakeService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("Sec-MS-GEC") != secMSGEC(s.clock) || q.Get("Sec-MS-GEC-Version") != tokenVersion {
		s.mu.Lock()
		s.refused++
		s.mu.Unlock()
		w.Header().Set("Date", s.clock.UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if r.Header.Get("Origin") != extensionOrigin || !strings.HasPrefix(r.Header.Get("Cookie"), "muid=") || len(q.Get("ConnectionId")) != 32 {
		s.t.Errorf("handshake lacks the browser identity: %v %v", r.Header, q)
	}
	upgrader := websocket.Upgrader{EnableCompression: true, CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.t.Error(err)
		return
	}
	defer conn.Close()
	_, config, err := conn.ReadMessage()
	if err != nil || !bytes.Contains(config, []byte("Path:speech.config")) || !bytes.Contains(config, []byte(outputFormat)) {
		s.t.Errorf("first frame is not the speech config: %q %v", config, err)
		return
	}
	_, ssml, err := conn.ReadMessage()
	if err != nil || !bytes.Contains(ssml, []byte("Path:ssml")) {
		s.t.Errorf("second frame is not the request: %q %v", ssml, err)
		return
	}
	_, body, _ := bytes.Cut(ssml, []byte("\r\n\r\n"))
	s.mu.Lock()
	s.requests = append(s.requests, string(body))
	n := len(s.requests)
	s.mu.Unlock()
	if bytes.Contains(body, []byte("silence")) {
		conn.WriteMessage(websocket.TextMessage, []byte("X-RequestId:abc\r\nPath:turn.end\r\n\r\n{}"))
		return
	}
	conn.WriteMessage(websocket.TextMessage, []byte("X-RequestId:abc\r\nPath:turn.start\r\n\r\n{}"))
	conn.WriteMessage(websocket.BinaryMessage, audioFrame(strings.Repeat("A", n)))
	conn.WriteMessage(websocket.TextMessage, []byte("X-RequestId:abc\r\nPath:audio.metadata\r\n\r\n{\"Metadata\":[]}"))
	conn.WriteMessage(websocket.BinaryMessage, audioFrame("mp3;"))
	conn.WriteMessage(websocket.TextMessage, []byte("X-RequestId:abc\r\nPath:turn.end\r\n\r\n{}"))
}

func newFake(t *testing.T, clock, local time.Time) (*fakeService, *Client) {
	service := &fakeService{t: t, clock: clock}
	server := httptest.NewServer(service)
	t.Cleanup(server.Close)
	return service, &Client{Endpoint: "ws" + strings.TrimPrefix(server.URL, "http"), Now: func() time.Time { return local }}
}

func TestSynthesizeStreamsEveryPieceAndLearnsTheServiceClock(t *testing.T) {
	clock := time.Unix(1794500000, 0)
	service, client := newFake(t, clock, clock.Add(-3*time.Hour))
	text := strings.Repeat("word ", 1000) + "a < b & c\vend"
	var out bytes.Buffer
	if err := client.Synthesize(context.Background(), text, Options{Voice: "en-US-AriaNeural", Rate: "+25%"}, &out); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "Amp3;AAmp3;"; got != want {
		t.Fatalf("audio = %q, want %q", got, want)
	}
	if service.refused != 1 {
		t.Fatalf("handshakes refused = %d, want one before the clock correction", service.refused)
	}
	last := service.requests[len(service.requests)-1]
	for _, want := range []string{"<voice name='Microsoft Server Speech Text to Speech Voice (en-US, AriaNeural)'>", "rate='+25%'", "a &lt; b &amp; c end</prosody>"} {
		if !strings.Contains(last, want) {
			t.Errorf("request %q lacks %q", last, want)
		}
	}
}

func TestSynthesizeReportsSilenceRefusalAndCancellation(t *testing.T) {
	clock := time.Unix(1794500000, 0)
	_, client := newFake(t, clock, clock)
	if err := client.Synthesize(context.Background(), "silence", Options{Voice: "en-US-AriaNeural"}, &bytes.Buffer{}); !errors.Is(err, ErrNoAudio) {
		t.Fatalf("silent turn error = %v, want ErrNoAudio", err)
	}
	if err := client.Synthesize(context.Background(), "hi", Options{Voice: "en-US-AriaNeural", Rate: "fast"}, &bytes.Buffer{}); err == nil {
		t.Fatal("a rate without a sign and percent passed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.Synthesize(ctx, "hi", Options{Voice: "en-US-AriaNeural"}, &bytes.Buffer{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled synthesis error = %v, want context.Canceled", err)
	}
	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer gone.Close()
	var status *StatusError
	down := &Client{Endpoint: "ws" + strings.TrimPrefix(gone.URL, "http")}
	if err := down.Synthesize(context.Background(), "hi", Options{Voice: "en-US-AriaNeural"}, &bytes.Buffer{}); !errors.As(err, &status) || status.Code != http.StatusServiceUnavailable {
		t.Fatalf("refused handshake error = %v, want status 503", err)
	}
}
