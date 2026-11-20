package edgetts

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

const (
	// Read aloud synthesis socket of the Edge browser
	DefaultEndpoint = "wss://speech.platform.bing.com/consumer/speech/synthesize/readaloud/edge/v1"

	chromiumVersion = "143.0.3650.75"
	chromiumMajor   = "143"
	tokenVersion    = "2-" + chromiumVersion
	extensionOrigin = "chrome-extension://jdiccldimpdaibmpdkjnbmckianbfold"
	userAgent       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + chromiumMajor + ".0.0.0 Safari/537.36 Edg/" + chromiumMajor + ".0.0.0"
	outputFormat    = "audio-24khz-48kbitrate-mono-mp3"
	chunkBytes      = 4096
	connectTimeout  = 10 * time.Second
	receiveTimeout  = 60 * time.Second
)

// Returned when the service ends a synthesis without sending sound
var ErrNoAudio = errors.New("edge speech service returned no audio, check the voice name and the text")

// HTTP status the service answered the socket handshake with
type StatusError struct {
	Code int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("edge speech service refused the connection with HTTP %d", e.Code)
}

// Voice and speaking rate of one synthesis
type Options struct {
	Voice string
	Rate  string
}

// Speaks text through the Edge read aloud service and keeps the clock correction it learned
type Client struct {
	Endpoint string
	Dialer   *websocket.Dialer
	Now      func() time.Time

	mu   sync.Mutex
	skew time.Duration
}

// Shared client of the production service
var Default = &Client{}

var (
	shortVoice = regexp.MustCompile(`^([a-z]{2,})-([A-Z]{2,})-(.+Neural)$`)
	fullVoice  = regexp.MustCompile(`^Microsoft Server Speech Text to Speech Voice \(.+,.+\)$`)
	ratePat    = regexp.MustCompile(`^[+-]\d+%$`)
	xmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
)

// Service spelling of a voice given as a short name like en-US-AriaNeural or already in full
func serviceVoice(voice string) (string, error) {
	if m := shortVoice.FindStringSubmatch(voice); m != nil {
		region, name := m[2], m[3]
		if at := strings.Index(name, "-"); at >= 0 {
			region, name = region+"-"+name[:at], name[at+1:]
		}
		voice = "Microsoft Server Speech Text to Speech Voice (" + m[1] + "-" + region + ", " + name + ")"
	}
	if !fullVoice.MatchString(voice) {
		return "", fmt.Errorf("invalid Edge voice %q, expected a name like en-US-AriaNeural", voice)
	}
	return voice, nil
}

// Text with the control characters the service rejects turned into spaces
func cleanText(text string) string {
	return strings.Map(func(r rune) rune {
		if r <= 8 || r == 11 || r == 12 || (r >= 14 && r <= 31) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(text, " "))
}

// Splits escaped text into pieces of at most limit bytes at a newline, a space or a rune boundary, never inside an entity
func splitText(text []byte, limit int) [][]byte {
	var out [][]byte
	for len(text) > limit {
		at := bytes.LastIndexByte(text[:limit], '\n')
		if at < 0 {
			at = bytes.LastIndexByte(text[:limit], ' ')
		}
		if at < 0 {
			at = limit
			for at > 0 && !utf8.RuneStart(text[at]) {
				at--
			}
		}
		for at > 0 {
			amp := bytes.LastIndexByte(text[:at], '&')
			if amp < 0 || bytes.IndexByte(text[amp:at], ';') >= 0 {
				break
			}
			at = amp
		}
		if piece := bytes.TrimSpace(text[:at]); len(piece) > 0 {
			out = append(out, piece)
		}
		if at == 0 {
			at = 1
		}
		text = text[at:]
	}
	if piece := bytes.TrimSpace(text); len(piece) > 0 {
		out = append(out, piece)
	}
	return out
}

func randomHex(n int) string {
	buf := make([]byte, n)
	rand.Read(buf)
	return hex.EncodeToString(buf)
}

func (c *Client) now() time.Time {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return now().Add(c.skew)
}

// Moves the client clock onto the service clock named by a refused handshake
func (c *Client) correctClock(resp *http.Response) error {
	server, err := http.ParseTime(resp.Header.Get("Date"))
	if err != nil {
		return fmt.Errorf("edge speech service refused the connection and sent no usable Date header: %w", &StatusError{Code: resp.StatusCode})
	}
	local := c.now()
	c.mu.Lock()
	c.skew += server.Sub(local)
	c.mu.Unlock()
	return nil
}

// Timestamp header value in the spelling the Edge browser sends
func stamp(at time.Time) string {
	return at.UTC().Format("Mon Jan 02 2006 15:04:05") + " GMT+0000 (Coordinated Universal Time)"
}

func (c *Client) dial(ctx context.Context) (*websocket.Conn, *http.Response, error) {
	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	query := url.Values{}
	query.Set("TrustedClientToken", trustedClientToken)
	query.Set("ConnectionId", randomHex(16))
	query.Set("Sec-MS-GEC", secMSGEC(c.now()))
	query.Set("Sec-MS-GEC-Version", tokenVersion)
	header := http.Header{}
	header.Set("Pragma", "no-cache")
	header.Set("Cache-Control", "no-cache")
	header.Set("Origin", extensionOrigin)
	header.Set("User-Agent", userAgent)
	header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	header.Set("Accept-Language", "en-US,en;q=0.9")
	header.Set("Cookie", "muid="+strings.ToUpper(randomHex(16))+";")
	dialer := c.Dialer
	if dialer == nil {
		dialer = &websocket.Dialer{Proxy: http.ProxyFromEnvironment, HandshakeTimeout: connectTimeout, EnableCompression: true}
	}
	return dialer.DialContext(ctx, endpoint+"?"+query.Encode(), header)
}

// Speaks one piece of escaped text over a fresh connection, writing the MP3 frames to w
func (c *Client) speak(ctx context.Context, voice, rate string, piece []byte, w io.Writer) error {
	conn, resp, err := c.dial(ctx)
	if err != nil {
		if resp != nil && errors.Is(err, websocket.ErrBadHandshake) {
			if resp.StatusCode != http.StatusForbidden {
				return &StatusError{Code: resp.StatusCode}
			}
			if err := c.correctClock(resp); err != nil {
				return err
			}
			if conn, resp, err = c.dial(ctx); err != nil {
				if resp != nil && errors.Is(err, websocket.ErrBadHandshake) {
					return &StatusError{Code: resp.StatusCode}
				}
				return err
			}
		} else {
			return err
		}
	}
	defer conn.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()
	at := stamp(c.now())
	config := "X-Timestamp:" + at + "\r\nContent-Type:application/json; charset=utf-8\r\nPath:speech.config\r\n\r\n" +
		`{"context":{"synthesis":{"audio":{"metadataoptions":{"sentenceBoundaryEnabled":"true","wordBoundaryEnabled":"false"},"outputFormat":"` + outputFormat + `"}}}}` + "\r\n"
	ssml := "X-RequestId:" + randomHex(16) + "\r\nContent-Type:application/ssml+xml\r\nX-Timestamp:" + at + "Z\r\nPath:ssml\r\n\r\n" +
		"<speak version='1.0' xmlns='http://www.w3.org/2001/10/synthesis' xml:lang='en-US'><voice name='" + voice + "'>" +
		"<prosody pitch='+0Hz' rate='" + rate + "' volume='+0%'>" + string(piece) + "</prosody></voice></speak>"
	for _, message := range []string{config, ssml} {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(message)); err != nil {
			return wireError(ctx, err)
		}
	}
	heard := false
	for {
		conn.SetReadDeadline(time.Now().Add(receiveTimeout))
		kind, data, err := conn.ReadMessage()
		if err != nil {
			return wireError(ctx, err)
		}
		switch kind {
		case websocket.TextMessage:
			head, _, _ := bytes.Cut(data, []byte("\r\n\r\n"))
			if frameHeader(head, "Path") == "turn.end" {
				if !heard {
					return ErrNoAudio
				}
				return nil
			}
		case websocket.BinaryMessage:
			if len(data) < 2 {
				return errors.New("edge speech service sent an audio frame without a header length")
			}
			size := int(binary.BigEndian.Uint16(data))
			if 2+size > len(data) {
				return errors.New("edge speech service sent an audio frame shorter than its header")
			}
			head, audio := data[2:2+size], data[2+size:]
			if path := frameHeader(head, "Path"); path != "audio" {
				return fmt.Errorf("edge speech service sent a binary frame for %q, expected audio", path)
			}
			if len(audio) == 0 {
				continue
			}
			if media := frameHeader(head, "Content-Type"); media != "audio/mpeg" {
				return fmt.Errorf("edge speech service sent audio as %q, expected audio/mpeg", media)
			}
			if _, err := w.Write(audio); err != nil {
				return err
			}
			heard = true
		}
	}
}

// Value of one header of a service frame, empty when the frame lacks it
func frameHeader(head []byte, name string) string {
	for _, line := range bytes.Split(head, []byte("\r\n")) {
		key, value, ok := bytes.Cut(line, []byte(":"))
		if ok && string(key) == name {
			return string(value)
		}
	}
	return ""
}

// Connection failure reported as the context's end when that is what closed the socket
func wireError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("edge speech service connection failed: %w", err)
}

// Speaks text in the chosen voice and writes one MP3 stream to w
func (c *Client) Synthesize(ctx context.Context, text string, opts Options, w io.Writer) error {
	voice, err := serviceVoice(opts.Voice)
	if err != nil {
		return err
	}
	rate := opts.Rate
	if rate == "" {
		rate = "+0%"
	}
	if !ratePat.MatchString(rate) {
		return fmt.Errorf("invalid Edge speaking rate %q, expected a signed percentage like +25%%", rate)
	}
	pieces := splitText([]byte(xmlEscaper.Replace(cleanText(text))), chunkBytes)
	if len(pieces) == 0 {
		return errors.New("no text to speak")
	}
	for _, piece := range pieces {
		if err := c.speak(ctx, voice, rate, piece, w); err != nil {
			return err
		}
	}
	return nil
}
