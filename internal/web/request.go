package web

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/net/html/charset"

	"github.com/nickheyer/icebath/internal/provider"
)

// Most bytes of an answer read
const maxBody = 64 << 20

// One call to a web backend
type Request struct {
	// GET when empty
	Method string
	URL    string
	Query  url.Values
	Header map[string]string
	// Sent as JSON when set
	Body any
	// Longest the call may take, no limit for zero
	Timeout time.Duration
}

type Response struct {
	Status int
	Header http.Header
	Body   []byte
	// Address that answered, after redirects
	URL string
}

// Headers a call may carry to another host it is redirected to, every other header it sets being taken for a credential
var plain = map[string]bool{"Accept": true, "Content-Type": true, "User-Agent": true}

// Sends the request and reads the whole answer. Redirects are followed, the headers the call sets going only to the host it was sent to
func Send(ctx context.Context, request Request) (*Response, error) {
	var body io.Reader
	if request.Body != nil {
		data, err := json.Marshal(request.Body)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	if request.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, request.Timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, cmp.Or(request.Method, http.MethodGet), request.URL, body)
	if err != nil {
		return nil, err
	}
	if len(request.Query) > 0 {
		query := req.URL.Query()
		for name, values := range request.Query {
			query[name] = values
		}
		req.URL.RawQuery = query.Encode()
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", provider.UserAgent("icebath"))
	if request.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range request.Header {
		req.Header.Set(name, value)
	}
	client := &http.Client{CheckRedirect: func(next *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if next.URL.Host != via[0].URL.Host {
			for name := range request.Header {
				if !plain[http.CanonicalHeaderKey(name)] {
					next.Header.Del(name)
				}
			}
		}
		return nil
	}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	return &Response{Status: resp.StatusCode, Header: resp.Header, Body: data, URL: resp.Request.URL.String()}, nil
}

func (r *Response) OK() bool {
	return r.Status >= 200 && r.Status < 300
}

// Answer as text, read in the charset its Content-Type names, else as UTF-8
func (r *Response) Text() string {
	return strings.ToValidUTF8(string(Decoded(r.Header.Get("Content-Type"), r.Body)), "�")
}

// Answer read as JSON into v
func (r *Response) Decode(v any) error {
	return json.Unmarshal(Decoded(r.Header.Get("Content-Type"), r.Body), v)
}

// What a failed answer said, its status when it said nothing
func (r *Response) Detail() string {
	if text := strings.TrimSpace(r.Text()); text != "" {
		return text
	}
	return fmt.Sprintf("HTTP %d", r.Status)
}

// Bytes in UTF-8 from the charset the content type names, as they are when it names UTF-8, none, or one no decoder knows
func Decoded(contentType string, body []byte) []byte {
	_, params, err := mime.ParseMediaType(contentType)
	label := strings.ToLower(params["charset"])
	if err != nil || label == "" || label == "utf-8" || label == "utf8" {
		return body
	}
	reader, err := charset.NewReaderLabel(label, bytes.NewReader(body))
	if err != nil {
		return body
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		return body
	}
	return decoded
}

var originError = regexp.MustCompile(`^HTTP \d{3} from origin(?::|$)`)

// Whether a page's error is the HTTP answer of the page itself, which no other backend would read differently
func Origin(message string) bool {
	return originError.MatchString(message)
}

// HTTP status a vendor gives for a page: a three digit code as a number, or as decimal text in any script
func Status(value any) (int, bool) {
	var code int
	switch v := value.(type) {
	case float64:
		if v != float64(int(v)) {
			return 0, false
		}
		code = int(v)
	case int:
		code = v
	case json.Number:
		n, err := strconv.Atoi(v.String())
		if err != nil {
			return 0, false
		}
		code = n
	case string:
		n, err := strconv.Atoi(asciiDigits(strings.TrimSpace(v)))
		if err != nil {
			return 0, false
		}
		code = n
	default:
		return 0, false
	}
	if code < 100 || code > 999 {
		return 0, false
	}
	return code, true
}

// Decimal digits of any script as ASCII digits
func asciiDigits(text string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x80 || !unicode.IsDigit(r) {
			return r
		}
		for _, span := range unicode.Nd.R16 {
			if uint16(r) >= span.Lo && uint16(r) <= span.Hi && r <= 0xFFFF {
				return '0' + (r-rune(span.Lo))%10
			}
		}
		for _, span := range unicode.Nd.R32 {
			if uint32(r) >= span.Lo && uint32(r) <= span.Hi {
				return '0' + (r-rune(span.Lo))%10
			}
		}
		return r
	}, text)
}

// Error naming the page's own HTTP answer when the status is 400 or above, the reason after it; empty for any other status
func OriginError(status any, reason string) string {
	code, ok := Status(status)
	if !ok || code < 400 {
		return ""
	}
	text := "HTTP " + strconv.Itoa(code) + " from origin"
	if reason = strings.TrimSpace(reason); reason != "" {
		return text + ": " + reason
	}
	return text
}

// The page marked as failed when its own answer was 400 or above, its body kept as content
func (p *Page) WithStatus(status any, reason string) *Page {
	if failure := OriginError(status, reason); failure != "" {
		p.Error = failure
	}
	return p
}

// Page a vendor listed as failed: the page's own answer when the vendor gives a usable status, else what the vendor said
func Refused(url string, status any, reason, body string) *Page {
	if failure := OriginError(status, ""); failure != "" {
		return &Page{URL: url, Error: failure, Content: body, Source: url}
	}
	return &Page{URL: url, Error: cmp.Or(body, reason, "extraction failed"), Source: url}
}

// Answer of a GET read as JSON into v, else the failed search to give back: the backend could not be reached, answered with a status
// that is not 2xx, or not with JSON. Reach names what could not be reached, the label when empty
func GetJSON(ctx context.Context, label, reach string, request Request, v any) *Results {
	response, err := Send(ctx, request)
	if err != nil {
		return Failed(fmt.Sprintf("Could not reach %s: %v", cmp.Or(reach, label), err))
	}
	if !response.OK() {
		return Failed(fmt.Sprintf("%s returned HTTP %d", label, response.Status))
	}
	if response.Decode(v) != nil {
		return Failed(fmt.Sprintf("Could not parse %s response as JSON", label))
	}
	return nil
}

// JSON value read as text: a string as it is, a number or boolean as written, null as nothing, an array or object as its JSON
type Text string

func (t *Text) UnmarshalJSON(data []byte) error {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	switch v := value.(type) {
	case nil:
		*t = ""
	case string:
		*t = Text(v)
	default:
		*t = Text(bytes.TrimSpace(data))
	}
	return nil
}

func (t Text) String() string { return string(t) }
