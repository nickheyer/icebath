package whatsapp

import (
	"container/list"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Extension to mime type of outbound media files
var bridgeMimeMap = map[string]string{
	"jpg": "image/jpeg", "jpeg": "image/jpeg", "png": "image/png", "webp": "image/webp", "gif": "image/gif",
	"mp4": "video/mp4", "mov": "video/quicktime", "avi": "video/x-msvideo", "mkv": "video/x-matroska", "3gp": "video/3gpp",
	"pdf": "application/pdf", "doc": "application/msword",
	"docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	"xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
}

var (
	bridgeDeviceSuffix   = regexp.MustCompile(`:\d+@`)
	bridgeTrailingDevice = regexp.MustCompile(`:\d+$`)
	bridgeNonDigits      = regexp.MustCompile(`\D`)
	bridgeUnsafeName     = regexp.MustCompile(`[^a-zA-Z0-9._-]`)
	bridgeVcardName      = regexp.MustCompile(`FN:(.+)`)
	bridgeVcardPhone     = regexp.MustCompile(`TEL[^:]*:(.+)`)
)

// WhatsApp id with the device suffix dropped so device qualified and bare forms compare equal
func bridgeNormalizeID(value string) string {
	if value == "" {
		return ""
	}
	if loc := bridgeDeviceSuffix.FindStringIndex(value); loc != nil {
		value = value[:loc[0]] + value[loc[1]-1:]
	}
	return bridgeTrailingDevice.ReplaceAllString(value, "")
}

// Bare identifier of a WhatsApp id, device, domain and leading plus removed
func normalizeWhatsAppIdentifier(value string) string {
	v := strings.TrimSpace(value)
	if i := strings.Index(v, ":"); i >= 0 {
		if j := strings.LastIndex(v, "@"); j > i {
			v = v[:i] + v[j:]
		}
	}
	if i := strings.Index(v, "@"); i >= 0 {
		v = v[:i]
	}
	return strings.TrimPrefix(v, "+")
}

// Allowlist set of a comma separated env value
func parseAllowedUsers(raw string) map[string]bool {
	out := map[string]bool{}
	for _, v := range strings.Split(raw, ",") {
		if n := normalizeWhatsAppIdentifier(v); n != "" {
			out[n] = true
		}
	}
	return out
}

// Identifier a lid-mapping file in the session maps to, empty when absent or unreadable
func readMappingFile(sessionDir, identifier, suffix string) string {
	raw, err := os.ReadFile(filepath.Join(sessionDir, "lid-mapping-"+identifier+suffix+".json"))
	if err != nil {
		return ""
	}
	parsed, err := pyjson.LoadsBytes(raw)
	if err != nil {
		return ""
	}
	return normalizeWhatsAppIdentifier(jsString(parsed))
}

// JavaScript String() of a decoded JSON value
func jsString(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return jsNumber(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			if e != nil {
				parts[i] = jsString(e)
			}
		}
		return strings.Join(parts, ",")
	}
	return "[object Object]"
}

// JavaScript number formatting of a float
func jsNumber(x float64) string {
	if x == math.Trunc(x) && math.Abs(x) < 1e21 {
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return strconv.FormatFloat(x, 'g', -1, 64)
}

// Every identifier reachable through the session lid-mapping files, the input included
func expandWhatsAppIdentifiers(identifier, sessionDir string) map[string]bool {
	resolved := map[string]bool{}
	normalized := normalizeWhatsAppIdentifier(identifier)
	if normalized == "" {
		return resolved
	}
	queue := []string{normalized}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if current == "" || resolved[current] {
			continue
		}
		resolved[current] = true
		for _, suffix := range []string{"", "_reverse"} {
			if mapped := readMappingFile(sessionDir, current, suffix); mapped != "" && !resolved[mapped] {
				queue = append(queue, mapped)
			}
		}
	}
	return resolved
}

// Whether a sender id or its alias matches the allowlist, star allowing everyone
func matchesAllowedUser(senderID string, allowed map[string]bool, sessionDir string) bool {
	if len(allowed) == 0 {
		return false
	}
	if allowed["*"] {
		return true
	}
	for alias := range expandWhatsAppIdentifiers(senderID, sessionDir) {
		if allowed[alias] {
			return true
		}
	}
	return false
}

// Whether the sender or its alternate id is allowed
func matchesAllowedSender(senderID, senderAltID string, allowed map[string]bool, sessionDir string) bool {
	return matchesAllowedUser(senderID, allowed, sessionDir) || matchesAllowedUser(senderAltID, allowed, sessionDir)
}

// Whether group intake is allowed by the group policy and the group allowlist
func matchesInboundWhatsAppGroup(chatID, groupPolicy string, groupAllowed map[string]bool, sessionDir string) bool {
	switch groupPolicy {
	case "disabled", "pairing":
		return false
	case "allowlist":
		return matchesAllowedUser(chatID, groupAllowed, sessionDir)
	}
	return groupPolicy == "open"
}

// Insertion ordered set of recent ids bounded to a capacity
type outboundIDTracker struct {
	mu    sync.Mutex
	max   int
	order *list.List
	ids   map[string]*list.Element
}

// Tracker of at most max ids
func newOutboundIDTracker(max int) *outboundIDTracker {
	if max < 1 {
		panic("createOutboundIdTracker: maxSize must be a positive integer")
	}
	return &outboundIDTracker{max: max, order: list.New(), ids: map[string]*list.Element{}}
}

// Adds an id, evicting the oldest past the capacity
func (t *outboundIDTracker) remember(id string) {
	if id == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.ids[id]; !ok {
		t.ids[id] = t.order.PushBack(id)
	}
	for len(t.ids) > t.max {
		oldest := t.order.Front()
		t.order.Remove(oldest)
		delete(t.ids, oldest.Value.(string))
	}
}

// Whether the id was remembered
func (t *outboundIDTracker) has(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.ids[id]
	return id != "" && ok
}

// Decision for a fromMe message in bot mode, echo and disabled and allowlist drops before forwarding
func classifyOwnerMessageGate(fromMe, fromOwnerEnabled bool, recentlySent *outboundIDTracker, allowlistMatches func(string) bool, messageID, chatID string) string {
	switch {
	case !fromMe:
		return "pass"
	case recentlySent != nil && recentlySent.has(messageID):
		return "drop_echo"
	case !fromOwnerEnabled:
		return "drop_disabled"
	case allowlistMatches != nil && !allowlistMatches(chatID):
		return "drop_allowlist"
	}
	return "forward_owner"
}

// Least recently used map bounded to a capacity, refreshed on read
type boundedLRU[V any] struct {
	mu    sync.Mutex
	limit int
	order *list.List
	byKey map[string]*list.Element
}

type lruEntry[V any] struct {
	key   string
	value V
}

// Store of at most limit entries
func newBoundedLRU[V any](limit int) *boundedLRU[V] {
	return &boundedLRU[V]{limit: limit, order: list.New(), byKey: map[string]*list.Element{}}
}

// Stores the value as the newest entry, evicting the oldest past the limit
func (s *boundedLRU[V]) put(key string, value V) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.byKey[key]; ok {
		s.order.Remove(el)
	}
	s.byKey[key] = s.order.PushBack(&lruEntry[V]{key, value})
	for len(s.byKey) > s.limit {
		oldest := s.order.Front()
		s.order.Remove(oldest)
		delete(s.byKey, oldest.Value.(*lruEntry[V]).key)
	}
}

// Value of the key, refreshed as newest when touch is set
func (s *boundedLRU[V]) get(key string, touch bool) (V, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	el, ok := s.byKey[key]
	if !ok {
		var zero V
		return zero, false
	}
	if touch {
		s.order.MoveToBack(el)
	}
	return el.Value.(*lruEntry[V]).value, true
}

// JavaScript string length in UTF-16 code units
func jsLen(s string) int { return len(utf16.Encode([]rune(s))) }

// Text split under the limit at a newline in the second half, else a space, else hard
func splitLongMessage(message string, maxLength int) []string {
	if message == "" {
		return []string{}
	}
	units := utf16.Encode([]rune(message))
	if maxLength < 1 || len(units) <= maxLength {
		return []string{message}
	}
	chunks := []string{}
	remaining := units
	lastIndex := func(ch uint16, from int) int {
		if from >= len(remaining) {
			from = len(remaining) - 1
		}
		for i := from; i >= 0; i-- {
			if remaining[i] == ch {
				return i
			}
		}
		return -1
	}
	for len(remaining) > maxLength {
		splitAt := lastIndex('\n', maxLength)
		if splitAt < maxLength/2 {
			splitAt = lastIndex(' ', maxLength)
		}
		if splitAt < 1 {
			splitAt = maxLength
		}
		chunks = append(chunks, strings.TrimRightFunc(string(utf16.Decode(remaining[:splitAt])), jsIsSpace))
		remaining = utf16.Encode([]rune(strings.TrimLeftFunc(string(utf16.Decode(remaining[splitAt:])), jsIsSpace)))
	}
	if len(remaining) > 0 {
		chunks = append(chunks, string(utf16.Decode(remaining)))
	}
	return chunks
}

// JavaScript trim whitespace class
func jsIsSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', ' ', ' ', ' ', ' ', ' ', ' ', '　', '\ufeff':
		return true
	}
	return r >= ' ' && r <= ' '
}

// Id shortened to its last four digits and domain for debug lines
func redactWhatsAppID(value string) string {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return ""
	}
	userPart, domainPart, _ := strings.Cut(raw, "@")
	if i := strings.Index(domainPart, "@"); i >= 0 {
		domainPart = domainPart[:i]
	}
	bare, _, _ := strings.Cut(userPart, ":")
	digits := bridgeNonDigits.ReplaceAllString(bare, "")
	src := bare
	if digits != "" {
		src = digits
	}
	suffix := src
	if u := utf16.Encode([]rune(src)); len(u) > 4 {
		suffix = string(utf16.Decode(u[len(u)-4:]))
	}
	out := "…"
	if suffix != "" {
		out = "…" + suffix
	}
	if domainPart != "" {
		out += "@" + domainPart
	}
	return out
}

// Extension of a mime type for cached media, the fallback for unknown types
func mediaExtForMime(mime, fallback string) string {
	normalized, _, _ := strings.Cut(mime, ";")
	ext := map[string]string{"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp", "image/gif": ".gif",
		"video/mp4": ".mp4", "video/quicktime": ".mov", "video/x-matroska": ".mkv", "audio/ogg": ".ogg",
		"audio/mp4": ".m4a", "audio/mpeg": ".mp3", "application/pdf": ".pdf"}[strings.ToLower(normalized)]
	if ext == "" {
		return fallback
	}
	return ext
}

// Random lowercase hex of n bytes
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Writes downloaded media into the cache directory under a random prefixed name and returns its path
func writeMediaFile(buffer []byte, dir, prefix, ext, fileName string) (string, error) {
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", err
	}
	safeName := ""
	if fileName != "" {
		safeName = "_" + bridgeUnsafeName.ReplaceAllString(filepath.Base(fileName), "_")
	}
	if safeName != "" && ext != "" && filepath.Ext(safeName) == "" {
		safeName += ext
	}
	tail := safeName
	if tail == "" {
		tail = ext
	}
	filePath := filepath.Join(dir, prefix+"_"+randomHex(6)+tail)
	return filePath, os.WriteFile(filePath, buffer, 0o666)
}

// Bridge media kind of a file extension
func inferMediaType(ext string) string {
	switch ext {
	case "jpg", "jpeg", "png", "webp", "gif":
		return "image"
	case "mp4", "mov", "avi", "mkv", "3gp":
		return "video"
	case "ogg", "opus", "mp3", "wav", "m4a":
		return "audio"
	}
	return "document"
}

// Location text of a location message
func formatLocationText(name, address string, lat, lng *float64, isLive bool) string {
	label := name
	if label == "" {
		label = address
	}
	kind := "Location"
	if isLive {
		kind = "Live location"
	}
	coords := ""
	if lat != nil && lng != nil {
		coords = jsNumber(*lat) + "," + jsNumber(*lng)
	}
	parts := []string{}
	for _, p := range []string{label, coords} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return "[" + kind + ": " + strings.Join(parts, " ") + "]"
}

// Contact text of a vcard contact message
func formatContactText(displayName, vcard string) string {
	name := displayName
	if name == "" {
		if m := bridgeVcardName.FindStringSubmatch(vcard); m != nil {
			name = m[1]
		}
	}
	if name == "" {
		name = "unknown"
	}
	phone := ""
	if m := bridgeVcardPhone.FindStringSubmatch(vcard); m != nil {
		phone = m[1]
	}
	parts := []string{name}
	if phone != "" {
		parts = append(parts, phone)
	}
	return "[Contact: " + strings.Join(parts, " ") + "]"
}

// Contacts text listing the display names, the count when none has one
func formatContactsText(names []string, count int) string {
	kept := []string{}
	for _, n := range names {
		if n != "" {
			kept = append(kept, n)
		}
	}
	if len(kept) == 0 {
		return fmt.Sprintf("[Contacts: %d]", count)
	}
	return "[Contacts: " + strings.Join(kept, ", ") + "]"
}

// Reaction text with its target message id
func formatReactionText(emoji, target string) string {
	if target != "" {
		return "[Reaction: " + emoji + " to " + target + "]"
	}
	return "[Reaction: " + emoji + "]"
}

// Poll text with its question and options
func formatPollText(question string, options []string) string {
	if question == "" {
		question = "poll"
	}
	if len(options) > 0 {
		return "[Poll: " + question + " Options: " + strings.Join(options, ", ") + "]"
	}
	return "[Poll: " + question + "]"
}

// Poll update text with its target poll id
func formatPollUpdateText(target string) string {
	if target != "" {
		return "[Poll update: " + target + "]"
	}
	return "[Poll update]"
}

// Body with a visible note for each media download that failed
func appendMediaFailureNote(content string, failures []string) string {
	if len(failures) == 0 {
		return content
	}
	notes := make([]string, len(failures))
	for i, t := range failures {
		notes[i] = "[" + t + " could not be downloaded]"
	}
	note := strings.Join(notes, " ")
	if content != "" {
		return content + "\n" + note
	}
	return note
}

// Validated poll question, options and selectable count of a send-poll request
func buildPollPayload(question any, options []any, selectableCount any) (string, []string, int, error) {
	cleanQuestion := strings.TrimSpace(jsTruthyString(question))
	cleanOptions := []string{}
	for _, o := range options {
		if s := strings.TrimSpace(jsTruthyString(o)); s != "" {
			cleanOptions = append(cleanOptions, s)
		}
	}
	if cleanQuestion == "" {
		return "", nil, 0, errors.New("question is required")
	}
	if len(cleanOptions) < 2 {
		return "", nil, 0, errors.New("at least two poll options are required")
	}
	if len(cleanOptions) > 12 {
		return "", nil, 0, errors.New("at most 12 poll options are supported")
	}
	count := 1
	if n := jsNumberOf(selectableCount); !math.IsNaN(n) && n != 0 {
		count = int(n)
	}
	count = max(1, min(count, len(cleanOptions)))
	return cleanQuestion, cleanOptions, count, nil
}

// Text of a JSON value, empty for a falsy one
func jsTruthyString(v any) string {
	if !jsTruthy(v) {
		return ""
	}
	return jsString(v)
}

// JavaScript truthiness of a decoded JSON value
func jsTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int64:
		return x != 0
	case float64:
		return x != 0 && !math.IsNaN(x)
	}
	return true
}

// Number() of JavaScript over a decoded JSON value, NaN when it does not convert
func jsNumberOf(v any) float64 {
	switch x := v.(type) {
	case nil:
		return 0
	case bool:
		if x {
			return 1
		}
		return 0
	case int64:
		return float64(x)
	case float64:
		return x
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return 0
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f
		}
		return math.NaN()
	case []any:
		if len(x) == 0 {
			return 0
		}
		if len(x) == 1 {
			return jsNumberOf(jsString(x[0]))
		}
	}
	return math.NaN()
}

// Validated coordinates of a send-location request
func buildLocationPayload(latitude, longitude any) (float64, float64, error) {
	lat, lon := jsNumberOf(latitude), jsNumberOf(longitude)
	if math.IsNaN(lat) || math.IsInf(lat, 0) || math.IsNaN(lon) || math.IsInf(lon, 0) {
		return 0, 0, errors.New("latitude and longitude must be numbers")
	}
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return 0, 0, errors.New("latitude/longitude out of range")
	}
	return lat, lon, nil
}

// Local time stamp in the asctime shape every log file uses
func formatLogStamp(t time.Time) string {
	return t.Format("2006-01-02 15:04:05") + fmt.Sprintf(",%03d", t.Nanosecond()/int(time.Millisecond))
}

// Console of the bridge, stamped log lines and bare JSON event lines on one writer
type bridgeConsole struct {
	mu  sync.Mutex
	out io.Writer
}

// Stamped line, an empty line staying bare
func (c *bridgeConsole) log(format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	c.mu.Lock()
	defer c.mu.Unlock()
	if text == "" {
		fmt.Fprintln(c.out)
		return
	}
	fmt.Fprintln(c.out, formatLogStamp(time.Now())+" "+text)
}

// Raw text written unchanged
func (c *bridgeConsole) raw(text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	io.WriteString(c.out, text)
}

// One compact JSON line without a stamp
func (c *bridgeConsole) jsonLine(payload *pyjson.Object) {
	text, err := pyjson.Dumps(payload, pyjson.Separators(",", ":"), pyjson.EnsureASCII(false))
	if err != nil {
		return
	}
	c.raw(text + "\n")
}

// Ordered JSON object of alternating keys and values
func jsonObj(kv ...any) *pyjson.Object {
	o := pyjson.NewObject()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}
