package weixin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

const (
	defaultBaseURL        = "https://ilinkai.weixin.qq.com"
	defaultCDNBaseURL     = "https://novac2c.cdn.weixin.qq.com/c2c"
	ilinkAppID            = "bot"
	channelVersion        = "2.2.0"
	ilinkAppClientVersion = (2 << 16) | (2 << 8) | 0

	epGetUpdates   = "ilink/bot/getupdates"
	epSendMessage  = "ilink/bot/sendmessage"
	epSendTyping   = "ilink/bot/sendtyping"
	epGetConfig    = "ilink/bot/getconfig"
	epGetUploadURL = "ilink/bot/getuploadurl"
	epGetBotQR     = "ilink/bot/get_bot_qrcode"
	epGetQRStatus  = "ilink/bot/get_qrcode_status"

	longPollTimeoutMS = 35_000
	apiTimeout        = 15 * time.Second
	configTimeout     = 10 * time.Second
	qrTimeout         = 35 * time.Second
	uploadTimeout     = 120 * time.Second

	sessionExpiredErrcode = -14
	rateLimitErrcode      = -2

	mediaImage = 1
	mediaVideo = 2
	mediaFile  = 3
	mediaVoice = 4

	itemText  = 1
	itemImage = 2
	itemVoice = 3
	itemFile  = 4
	itemVideo = 5

	msgTypeBot     = 2
	msgStateFinish = 2
	typingStart    = 1
	typingStop     = 2
)

// Hosts inbound full_url media may be fetched from, every other host refused to prevent SSRF
var cdnAllowlist = map[string]bool{
	"novac2c.cdn.weixin.qq.com": true, "ilinkai.weixin.qq.com": true, "wx.qlogo.cn": true, "thirdwx.qlogo.cn": true,
	"res.wx.qq.com": true, "mmbiz.qpic.cn": true, "mmbiz.qlogo.cn": true,
}

// JSON scalar read as text since the API mixes strings and numbers, null and absent reading empty
type flexString string

func (s *flexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case len(b) == 0 || string(b) == "null" || string(b) == "false":
		*s = ""
	case b[0] == '"':
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*s = flexString(v)
	default:
		*s = flexString(b)
	}
	return nil
}

// Value with surrounding whitespace removed
func (s flexString) trimmed() string { return strings.TrimSpace(string(s)) }

// JSON number read as an integer, any other value reading zero
type flexInt int64

func (n *flexInt) UnmarshalJSON(b []byte) error {
	var f float64
	if json.Unmarshal(b, &f) == nil && f == math.Trunc(f) {
		*n = flexInt(f)
	} else {
		*n = 0
	}
	return nil
}

// Raw ret or errcode value of an iLink reply, nil when absent or null
type statusCode struct{ raw string }

func (c *statusCode) UnmarshalJSON(b []byte) error {
	c.raw = string(bytes.TrimSpace(b))
	return nil
}

// Numeric value of the code and whether it is a number at all
func (c *statusCode) value() (float64, bool) {
	if c == nil {
		return 0, false
	}
	switch c.raw {
	case "true":
		return 1, true
	case "false":
		return 0, true
	}
	f, err := strconv.ParseFloat(c.raw, 64)
	return f, err == nil
}

// Whether the code is absent or zero
func (c *statusCode) zero() bool {
	if c == nil {
		return true
	}
	v, ok := c.value()
	return ok && v == 0
}

// Whether the code equals n
func (c *statusCode) is(n int) bool {
	v, ok := c.value()
	return ok && v == float64(n)
}

func (c *statusCode) String() string {
	if c == nil {
		return "none"
	}
	if strings.HasPrefix(c.raw, `"`) {
		if s, err := strconv.Unquote(c.raw); err == nil {
			return s
		}
	}
	return c.raw
}

// Status fields every iLink reply may carry
type apiStatus struct {
	Ret     *statusCode `json:"ret"`
	Errcode *statusCode `json:"errcode"`
	Errmsg  flexString  `json:"errmsg"`
	Msg     flexString  `json:"msg"`
}

// Whether ret and errcode are both absent or zero
func (s *apiStatus) ok() bool { return s.Ret.zero() && s.Errcode.zero() }

// Error message of the reply, errmsg first then msg
func (s *apiStatus) message() string {
	if s.Errmsg != "" {
		return string(s.Errmsg)
	}
	return string(s.Msg)
}

// Whether ret or errcode is the frequency limit code
func (s *apiStatus) rateLimited() bool {
	return s.Ret.is(rateLimitErrcode) || s.Errcode.is(rateLimitErrcode)
}

// Whether a frequency limit reply is really a stale session that will not prepare a send
func (s *apiStatus) staleSession() bool {
	switch strings.ToLower(s.message()) {
	case "unknown error", "prepare failed":
		return s.rateLimited()
	}
	return false
}

// Whether the reply reports an expired or stale bot session
func (s *apiStatus) sessionExpired() bool {
	return s.Ret.is(sessionExpiredErrcode) || s.Errcode.is(sessionExpiredErrcode) || s.staleSession()
}

// Errmsg of the reply or the fallback when it has none
func (s *apiStatus) messageOr(fallback string) string {
	if m := s.message(); m != "" {
		return m
	}
	return fallback
}

// Deterministic failure of a send iLink will not prepare until the peer messages the bot again
func sessionNotReadyError(s *apiStatus) error {
	return fmt.Errorf("iLink sendmessage session not ready: ret=%s errcode=%s errmsg=%s"+
		" — the user must send the bot a message first (or re-pair)", s.Ret, s.Errcode, s.messageOr("unknown error"))
}

// Generic sendmessage failure of a reply
func sendMessageError(s *apiStatus) error {
	return fmt.Errorf("iLink sendmessage error: ret=%s errcode=%s errmsg=%s", s.Ret, s.Errcode, s.messageOr("unknown error"))
}

// Whether err is a request deadline rather than a cancellation or transport failure
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// First n code points of s
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// Percent encoding of every byte outside the unreserved set, spaces included
func quoteAll(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }

// Header map assignment preserving the exact header name case iLink documents
func setHeader(h http.Header, name, value string) { h[name] = []string{value} }

// Authenticated iLink POST header set with a fresh random UIN
func postHeaders(token string, body []byte) http.Header {
	var n [4]byte
	_, _ = rand.Read(n[:])
	uin := base64.StdEncoding.EncodeToString([]byte(strconv.FormatUint(uint64(binary.BigEndian.Uint32(n[:])), 10)))
	h := http.Header{}
	setHeader(h, "Content-Type", "application/json")
	setHeader(h, "AuthorizationType", "ilink_bot_token")
	setHeader(h, "Content-Length", strconv.Itoa(len(body)))
	setHeader(h, "X-WECHAT-UIN", uin)
	setHeader(h, "iLink-App-Id", ilinkAppID)
	setHeader(h, "iLink-App-ClientVersion", strconv.Itoa(ilinkAppClientVersion))
	if token != "" {
		setHeader(h, "Authorization", "Bearer "+token)
	}
	return h
}

// App identity header set of an unauthenticated iLink GET
func getHeaders() http.Header {
	h := http.Header{}
	setHeader(h, "iLink-App-Id", ilinkAppID)
	setHeader(h, "iLink-App-ClientVersion", strconv.Itoa(ilinkAppClientVersion))
	return h
}

// Compact JSON of the payload object with the channel base_info appended last
func encodeBody(payload any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return nil, err
	}
	obj := bytes.TrimSpace(buf.Bytes())
	if len(obj) < 2 || obj[0] != '{' || obj[len(obj)-1] != '}' {
		return nil, fmt.Errorf("iLink payload must be a JSON object")
	}
	base := `"base_info":{"channel_version":"` + channelVersion + `"}`
	inner := bytes.TrimSpace(obj[1 : len(obj)-1])
	if len(inner) == 0 {
		return []byte("{" + base + "}"), nil
	}
	return []byte("{" + string(inner) + "," + base + "}"), nil
}

// One iLink request under its own timeout, the raw body or the HTTP status error
func ilinkRequest(ctx context.Context, client *http.Client, method, baseURL, endpoint string, header http.Header, body []byte, timeout time.Duration) ([]byte, error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(rctx, method, strings.TrimRight(baseURL, "/")+"/"+endpoint, reader)
	if err != nil {
		return nil, err
	}
	req.Header = header
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("iLink %s %s HTTP %d: %s", method, endpoint, resp.StatusCode, truncateRunes(string(raw), 200))
	}
	return raw, nil
}

// Authenticated POST of a payload to an endpoint, the reply decoded into out and its raw body returned
func apiPost(ctx context.Context, client *http.Client, baseURL, endpoint, token string, payload, out any, timeout time.Duration) ([]byte, error) {
	body, err := encodeBody(payload)
	if err != nil {
		return nil, err
	}
	raw, err := ilinkRequest(ctx, client, http.MethodPost, baseURL, endpoint, postHeaders(token, body), body, timeout)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return raw, fmt.Errorf("iLink %s returned invalid JSON: %w", endpoint, err)
	}
	return raw, nil
}

// Unauthenticated GET of an endpoint with its query, the reply decoded into out
func apiGet(ctx context.Context, client *http.Client, baseURL, endpoint string, out any, timeout time.Duration) error {
	raw, err := ilinkRequest(ctx, client, http.MethodGet, baseURL, endpoint, getHeaders(), nil, timeout)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("iLink %s returned invalid JSON: %w", endpoint, err)
	}
	return nil
}

// Reply of a getupdates long poll
type updatesResponse struct {
	apiStatus
	Msgs                 []json.RawMessage `json:"msgs"`
	GetUpdatesBuf        flexString        `json:"get_updates_buf"`
	LongpollingTimeoutMS json.RawMessage   `json:"longpolling_timeout_ms"`
}

// Server suggested long poll timeout in milliseconds, zero unless a positive integer
func (r *updatesResponse) suggestedTimeoutMS() int {
	n, err := strconv.ParseInt(string(bytes.TrimSpace(r.LongpollingTimeoutMS)), 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return int(n)
}

// Long polls for new messages from the cursor, a client timeout reading as an empty batch
func getUpdates(ctx context.Context, client *http.Client, baseURL, token, syncBuf string, timeoutMS int) (*updatesResponse, error) {
	payload := struct {
		GetUpdatesBuf string `json:"get_updates_buf"`
	}{syncBuf}
	var resp updatesResponse
	_, err := apiPost(ctx, client, baseURL, epGetUpdates, token, payload, &resp, time.Duration(timeoutMS)*time.Millisecond)
	if err != nil && ctx.Err() == nil && isTimeout(err) {
		return &updatesResponse{GetUpdatesBuf: flexString(syncBuf)}, nil
	}
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

// CDN reference of an uploaded or downloadable media object
type cdnMedia struct {
	EncryptQueryParam flexString `json:"encrypt_query_param"`
	AESKey            flexString `json:"aes_key"`
	EncryptType       flexInt    `json:"encrypt_type,omitempty"`
	FullURL           flexString `json:"full_url,omitempty"`
	fields            int
}

func (m *cdnMedia) UnmarshalJSON(b []byte) error {
	type plain cdnMedia
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		return err
	}
	if err := json.Unmarshal(b, (*plain)(m)); err != nil {
		return err
	}
	m.fields = len(keys)
	return nil
}

// Whether the media block is present with any field
func (m *cdnMedia) present() bool { return m != nil && m.fields > 0 }

// Media payload of an image, voice, file or video item in either direction
type mediaPayload struct {
	Media    *cdnMedia  `json:"media,omitempty"`
	FileName flexString `json:"file_name,omitempty"`
	AESKey   flexString `json:"aeskey,omitempty"`
	Text     flexString `json:"text,omitempty"`
}

// Text payload of a text item
type textPayload struct {
	Text flexString `json:"text"`
}

// Quoted message attached to an item
type refMessage struct {
	Title       flexString   `json:"title"`
	MessageItem *messageItem `json:"message_item"`
}

// One entry of an inbound item_list
type messageItem struct {
	Type      flexInt       `json:"type"`
	TextItem  *textPayload  `json:"text_item"`
	ImageItem *mediaPayload `json:"image_item"`
	VoiceItem *mediaPayload `json:"voice_item"`
	FileItem  *mediaPayload `json:"file_item"`
	VideoItem *mediaPayload `json:"video_item"`
	RefMsg    *refMessage   `json:"ref_msg"`
	fields    int
}

func (m *messageItem) UnmarshalJSON(b []byte) error {
	type plain messageItem
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		return err
	}
	if err := json.Unmarshal(b, (*plain)(m)); err != nil {
		return err
	}
	m.fields = len(keys)
	return nil
}

// Whether a quoted item is present with any field
func (m *messageItem) present() bool { return m != nil && m.fields > 0 }

// Media payload of the item for its type, nil for text and unknown types
func (m *messageItem) mediaPayload() *mediaPayload {
	switch m.Type {
	case itemImage:
		return m.ImageItem
	case itemVoice:
		return m.VoiceItem
	case itemFile:
		return m.FileItem
	case itemVideo:
		return m.VideoItem
	}
	return nil
}

// One inbound message of a getupdates batch
type inboundMessage struct {
	FromUserID   flexString    `json:"from_user_id"`
	ToUserID     flexString    `json:"to_user_id"`
	MessageID    flexString    `json:"message_id"`
	RoomID       flexString    `json:"room_id"`
	ChatRoomID   flexString    `json:"chat_room_id"`
	MsgType      flexInt       `json:"msg_type"`
	ContextToken flexString    `json:"context_token"`
	ItemList     []messageItem `json:"item_list"`
}

// One outbound item of a sendmessage item_list
type outboundItem struct {
	Type      int          `json:"type"`
	TextItem  *textPayload `json:"text_item,omitempty"`
	ImageItem *imageOut    `json:"image_item,omitempty"`
	VoiceItem *voiceOut    `json:"voice_item,omitempty"`
	FileItem  *fileOut     `json:"file_item,omitempty"`
	VideoItem *videoOut    `json:"video_item,omitempty"`
}

// Outbound image fields after the shared media block
type imageOut struct {
	Media   cdnMedia `json:"media"`
	MidSize int      `json:"mid_size"`
}

// Outbound voice fields after the shared media block
type voiceOut struct {
	Media         cdnMedia `json:"media"`
	EncodeType    int      `json:"encode_type"`
	BitsPerSample int      `json:"bits_per_sample"`
	SampleRate    int      `json:"sample_rate"`
	Playtime      int      `json:"playtime"`
}

// Outbound file fields after the shared media block
type fileOut struct {
	Media    cdnMedia `json:"media"`
	FileName string   `json:"file_name"`
	Len      string   `json:"len"`
}

// Outbound video fields after the shared media block
type videoOut struct {
	Media      cdnMedia `json:"media"`
	VideoSize  int      `json:"video_size"`
	PlayLength int      `json:"play_length"`
	VideoMD5   string   `json:"video_md5"`
}

// Outbound item carrying plain text
func textItem(text string) outboundItem {
	return outboundItem{Type: itemText, TextItem: &textPayload{Text: flexString(text)}}
}

// Posts the items as one sendmessage to a peer, echoing the context token when there is one
func sendItems(ctx context.Context, client *http.Client, baseURL, token, to string, items []outboundItem, contextToken, clientID string) (*apiStatus, error) {
	type message struct {
		FromUserID   string         `json:"from_user_id"`
		ToUserID     string         `json:"to_user_id"`
		ClientID     string         `json:"client_id"`
		MessageType  int            `json:"message_type"`
		MessageState int            `json:"message_state"`
		ItemList     []outboundItem `json:"item_list"`
		ContextToken string         `json:"context_token,omitempty"`
	}
	payload := struct {
		Msg message `json:"msg"`
	}{message{ToUserID: to, ClientID: clientID, MessageType: msgTypeBot, MessageState: msgStateFinish, ItemList: items, ContextToken: contextToken}}
	var status apiStatus
	if _, err := apiPost(ctx, client, baseURL, epSendMessage, token, payload, &status, apiTimeout); err != nil {
		return nil, err
	}
	return &status, nil
}

// Ticket from getconfig authorizing typing indicators for a peer, empty when none was issued
func getTypingTicket(ctx context.Context, client *http.Client, baseURL, token, userID, contextToken string) (string, error) {
	payload := struct {
		IlinkUserID  string `json:"ilink_user_id"`
		ContextToken string `json:"context_token,omitempty"`
	}{userID, contextToken}
	var resp struct {
		TypingTicket flexString `json:"typing_ticket"`
	}
	if _, err := apiPost(ctx, client, baseURL, epGetConfig, token, payload, &resp, configTimeout); err != nil {
		return "", err
	}
	return string(resp.TypingTicket), nil
}

// Starts or stops the typing indicator for a peer
func sendTyping(ctx context.Context, client *http.Client, baseURL, token, userID, ticket string, status int) error {
	payload := struct {
		IlinkUserID  string `json:"ilink_user_id"`
		TypingTicket string `json:"typing_ticket"`
		Status       int    `json:"status"`
	}{userID, ticket, status}
	var resp apiStatus
	_, err := apiPost(ctx, client, baseURL, epSendTyping, token, payload, &resp, configTimeout)
	return err
}

// Destination iLink grants for one media object upload
type uploadSlot struct {
	UploadParam   flexString `json:"upload_param"`
	UploadFullURL flexString `json:"upload_full_url"`
	raw           string
}

// Upload slot for an encrypted media object addressed to a peer
func getUploadURL(ctx context.Context, client *http.Client, baseURL, token, toUserID string, mediaType int, filekey string, rawsize int, rawMD5 string, filesize int, aesKeyHex string) (*uploadSlot, error) {
	payload := struct {
		Filekey     string `json:"filekey"`
		MediaType   int    `json:"media_type"`
		ToUserID    string `json:"to_user_id"`
		Rawsize     int    `json:"rawsize"`
		RawFileMD5  string `json:"rawfilemd5"`
		Filesize    int    `json:"filesize"`
		NoNeedThumb bool   `json:"no_need_thumb"`
		AESKey      string `json:"aeskey"`
	}{filekey, mediaType, toUserID, rawsize, rawMD5, filesize, true, aesKeyHex}
	var slot uploadSlot
	raw, err := apiPost(ctx, client, baseURL, epGetUploadURL, token, payload, &slot, apiTimeout)
	if err != nil {
		return nil, err
	}
	slot.raw = string(raw)
	return &slot, nil
}

// Posts ciphertext to the CDN and returns the encrypted query param naming the stored object
func uploadCiphertext(ctx context.Context, client *http.Client, uploadURL string, ciphertext []byte) (string, error) {
	rctx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, uploadURL, bytes.NewReader(ciphertext))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if param := resp.Header.Get("x-encrypted-param"); resp.StatusCode == http.StatusOK && param != "" {
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			return "", err
		}
		return param, nil
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	text := truncateRunes(string(raw), 200)
	if resp.StatusCode == http.StatusOK {
		return "", fmt.Errorf("CDN upload missing x-encrypted-param header: %s", text)
	}
	return "", fmt.Errorf("CDN upload HTTP %d: %s", resp.StatusCode, text)
}

// Body of a GET under the timeout, an HTTP error status failing it
func downloadBytes(ctx context.Context, client *http.Client, rawURL string, timeout time.Duration) ([]byte, error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("download HTTP %d %s from %s", resp.StatusCode, http.StatusText(resp.StatusCode), gateway.SafeURLForLog(rawURL, 80))
	}
	return io.ReadAll(resp.Body)
}

// Refuses a full_url outside http and https or outside the WeChat CDN hosts
func checkCDNURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("unparseable media URL %q", rawURL)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("media URL has disallowed scheme %q, only http and https are permitted", scheme)
	}
	if host := strings.ToLower(u.Hostname()); !cdnAllowlist[host] {
		return fmt.Errorf("media URL host %q is not in the WeChat CDN allowlist, refusing to fetch it", host)
	}
	return nil
}

// Downloads one media object by encrypted query param or allowlisted full URL and decrypts it when keyed
func downloadMedia(ctx context.Context, client *http.Client, cdnBaseURL string, media *cdnMedia, key []byte, timeout time.Duration) ([]byte, error) {
	var target string
	switch {
	case media != nil && media.EncryptQueryParam != "":
		target = strings.TrimRight(cdnBaseURL, "/") + "/download?encrypted_query_param=" + quoteAll(string(media.EncryptQueryParam))
	case media != nil && media.FullURL != "":
		if err := checkCDNURL(string(media.FullURL)); err != nil {
			return nil, err
		}
		target = string(media.FullURL)
	default:
		return nil, errors.New("media item had neither encrypt_query_param nor full_url")
	}
	raw, err := downloadBytes(ctx, client, target, timeout)
	if err != nil {
		return nil, err
	}
	if key == nil {
		return raw, nil
	}
	return decryptECB(raw, key)
}
