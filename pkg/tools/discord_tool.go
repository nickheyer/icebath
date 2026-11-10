package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// Discord REST base URL, a seam tests point at a local stand in
var DiscordAPIBase = "https://discord.com/api/v10"

// Application flag bits of GET /applications/@me, each pairing an intent with its under 100 guild variant
const (
	DiscordFlagsGuildMembers   = 1<<14 | 1<<15
	DiscordFlagsMessageContent = 1<<18 | 1<<19
)

var (
	discordResponseBodyMaxBytes = 4 * 1024 * 1024
	discordErrorBodyMaxBytes    = 64 * 1024
)

// HTTP status and body of a Discord REST call that failed
type DiscordAPIError struct {
	Status int
	Body   string
}

func (e *DiscordAPIError) Error() string {
	return fmt.Sprintf("Discord API error %d: %s", e.Status, e.Body)
}

// ValueError of a Discord request, a header value http.client refuses or a body that is not JSON
type DiscordValueError struct{ Msg string }

func (e *DiscordValueError) Error() string { return e.Msg }

// OSError of a Discord request that never got an answer, its text the Python str of the exception
type DiscordOSError struct{ Text string }

func (e *DiscordOSError) Error() string { return e.Text }

// Sends one Discord request with a total timeout, a seam tests replace
var discordHTTPDo = func(req *http.Request, timeout time.Duration) (*http.Response, error) {
	return (&http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: pyurl.UrlopenProxy}}).Do(req)
}

// Header value http.client refuses, a bare newline or carriage return not starting a folded continuation
var illegalHeaderValue = regexp.MustCompile("\n[^ \t]|\n$|\r[^ \t\n]|\r$")

// Reads at most limit bytes of a body, the 502 Discord API error when it holds more
func readLimitedResponseBody(r io.Reader, limit int, label string) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > limit {
		return nil, &DiscordAPIError{Status: 502, Body: fmt.Sprintf("Discord API %s exceeded %d bytes.", label, limit)}
	}
	return body, nil
}

// Calls the Discord REST API with a bot token, nil for 204 and the decoded JSON otherwise
func DiscordRequest(ctx context.Context, method, path, token string, params [][2]string, body any, timeout time.Duration) (any, error) {
	url := DiscordAPIBase + path
	if len(params) > 0 {
		url += "?" + pyurl.URLEncode(params)
	}
	authorization := "Bot " + token
	if illegalHeaderValue.MatchString(authorization) {
		return nil, &DiscordValueError{Msg: "Invalid header value " + pyrepr.Repr([]byte(authorization))}
	}
	var payload io.Reader
	if body != nil {
		raw, err := pyjson.EncodeWTF8PythonASCII(body)
		if err != nil {
			return nil, err
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, payload)
	if err != nil {
		return nil, &DiscordValueError{Msg: err.Error()}
	}
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "icebath-agent (https://github.com/NousResearch/hermes-agent)")
	resp, err := discordHTTPDo(req, timeout)
	if err != nil {
		return nil, &DiscordOSError{Text: "<urlopen error " + discordTransportReason(err) + ">"}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, err := readLimitedResponseBody(resp.Body, discordErrorBodyMaxBytes, "error body")
		var tooLarge *DiscordAPIError
		errorBody := ""
		switch {
		case errors.As(err, &tooLarge):
			errorBody = tooLarge.Body
		case err == nil:
			errorBody = pystr.DecodeUTF8Replace(raw)
		}
		return nil, &DiscordAPIError{Status: resp.StatusCode, Body: errorBody}
	}
	if resp.StatusCode == 204 {
		return nil, nil
	}
	raw, err := readLimitedResponseBody(resp.Body, discordResponseBodyMaxBytes, "response body")
	if err != nil {
		return nil, err
	}
	text, err := pystr.DecodeUTF8(raw)
	if err != nil {
		return nil, &DiscordValueError{Msg: err.Error()}
	}
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return nil, &DiscordValueError{Msg: err.Error()}
	}
	return decoded, nil
}

// Reason text of a transport failure the way urllib's URLError shows its socket error
func discordTransportReason(err error) string {
	if class := pypath.OSErrorClass(err); class != "OSError" {
		return pypath.OSErrorText(err)
	}
	return err.Error()
}
