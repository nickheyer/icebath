package buzz

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/tools"

	"github.com/nickheyer/icebath/pkg/internal/textutil"
)

const (
	chatKind                   = 9
	fetchLimit                 = 50
	dmDiscoveryEvery           = 5
	defaultPollInterval        = 4.0
	minPollInterval            = 1.0
	cliTimeout                 = 30 * time.Second
	memberCacheTTL             = 60 * time.Second
	profileNameTTL             = 300 * time.Second
	maxInboundAttachments      = 4
	maxInboundAttachmentBytes  = 20 * 1024 * 1024
	attachmentDownloadTimeout  = 30 * time.Second
	maxAttachmentFilenameBytes = 120
	wsAuthTimeout              = 20 * time.Second
	wsReadIdleTimeout          = 300 * time.Second
	wsMaxMessageBytes          = 2_000_000
	wsPingInterval             = 20 * time.Second
	wsMembershipKind           = 44100
	wsMembershipSubID          = "icebath-buzz-membership"
	maxCLIMessageChars         = 900
	eventMetaContentCap        = 500
	threadRootCache            = 512
	presentationMentionSep     = "​"
)

// Event kinds that carry conversation content, chat messages plus forum posts and their comments
var dispatchKinds = map[int64]bool{chatKind: true, 45001: true, 45003: true}

// Dispatch kinds in ascending order for relay subscription filters
var dispatchKindList = []int64{chatKind, 45001, 45003}

var (
	unresolvedMentionErrorRE = regexp.MustCompile(`mention '@([^']+)' does not match a current channel member`)
	hex64RE                  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	mediaURLPattern          = `https?://[^\s<>\[\]()]+/media/[0-9a-f]{64}(?:\.[a-z0-9]{1,10})?(?:\?[^\s<>\[\]()]*)?`
	markdownMediaRE          = regexp.MustCompile(`(?i)!\[([^\]]*)\]\(\s*(` + mediaURLPattern + `)(?:\s+["'][^"']*["'])?\s*\)`)
	bareMediaRE              = regexp.MustCompile(`(?i)` + mediaURLPattern)
	mediaPathRE              = regexp.MustCompile(`(?i)^/media/([0-9a-f]{64})(\.[a-z0-9]{1,10})?/?$`)
	trailingSpaceLineRE      = regexp.MustCompile(`[ \t]+\n`)
	manyNewlinesRE           = regexp.MustCompile(`\n{3,}`)
)

// Compiled Python compatible pattern with lookaround, panicking on a pattern bug
func mustRE2(pattern string, opts regexp2.RegexOptions) *regexp2.Regexp {
	return regexp2.MustCompile(pattern, opts)
}

// Whether the Python compatible pattern matches anywhere in the text
func re2Match(re *regexp2.Regexp, text string) bool {
	ok, err := re.MatchString(text)
	return err == nil && ok
}

// Content with a CLI rejected @name made presentation only by an invisible separator after the @, false when nothing changed
func escapeUnresolvedPresentationMention(content, cliError string) (string, bool) {
	m := unresolvedMentionErrorRE.FindStringSubmatch(cliError)
	if m == nil || m[1] == "" {
		return "", false
	}
	token := mustRE2(`(?<!\S)@`+regexp2.Escape(m[1])+`(?=$|[^A-Za-z0-9._-])`, regexp2.IgnoreCase)
	count := 0
	escaped, err := token.ReplaceFunc(content, func(match regexp2.Match) string {
		count++
		return "@" + presentationMentionSep + match.String()[1:]
	}, -1, -1)
	if err != nil || count == 0 {
		return "", false
	}
	return escaped, true
}

// Final dotted suffix of a file name, empty for dotfiles and names ending in a dot
func pathSuffix(name string) string {
	i := strings.LastIndex(name, ".")
	if i <= 0 || i == len(name)-1 {
		return ""
	}
	return name[i:]
}

// Basename safe for cache files and agent context, control characters removed and the stem cut to the byte budget
func safeAttachmentFilename(value string) string {
	name := strings.ReplaceAll(value, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		if r >= 32 && r != 0x7f {
			b.WriteRune(r)
		}
	}
	name = strings.TrimSpace(b.String())
	if name == "" || name == "." || name == ".." {
		return "attachment.bin"
	}
	suffix := pathSuffix(name)
	if len(suffix) > 20 {
		suffix = ""
	}
	stem := strings.TrimSuffix(name, suffix)
	if suffix == "" {
		stem = name
	}
	budget := maxAttachmentFilenameBytes - len(suffix)
	if budget < 0 {
		budget = 0
	}
	if len(stem) > budget {
		stem = stem[:budget]
	}
	safe := strings.TrimRight(strings.ToValidUTF8(stem, ""), " .")
	if safe == "" {
		safe = "attachment"
	}
	return safe + suffix
}

// Host and port of an HTTPS equivalent origin
type origin struct {
	host string
	port int
}

// Exact HTTPS equivalent origin of a configured host or URL, false when it is not one
func attachmentOrigin(value string) (origin, bool) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return origin{}, false
	}
	if !strings.Contains(raw, "://") {
		raw = "//" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return origin{}, false
	}
	host := strings.TrimRight(strings.ToLower(parsed.Hostname()), ".")
	port := 443
	if p := parsed.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 65535 {
			return origin{}, false
		}
		port = n
	}
	if (parsed.Scheme != "" && parsed.Scheme != "https" && parsed.Scheme != "wss") || host == "" {
		return origin{}, false
	}
	return origin{host, port}, true
}

// Explicit port of a URL or the default of its scheme, false for a malformed port or unknown scheme
func effectivePort(u *url.URL) (int, bool) {
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 65535 {
			return 0, false
		}
		return n, true
	}
	n, ok := map[string]int{"https": 443, "wss": 443, "http": 80, "ws": 80}[u.Scheme]
	return n, ok
}

// Whether the URL is a Buzz media object on the configured relay
func isRelayMediaURL(raw, relayURL string) bool {
	candidate, err := url.Parse(raw)
	if err != nil {
		return false
	}
	relay, err := url.Parse(relayURL)
	if err != nil {
		return false
	}
	if candidate.Scheme != "http" && candidate.Scheme != "https" {
		return false
	}
	if candidate.Hostname() == "" || relay.Hostname() == "" || !strings.EqualFold(candidate.Hostname(), relay.Hostname()) {
		return false
	}
	cp, cok := effectivePort(candidate)
	rp, rok := effectivePort(relay)
	if cok != rok || cp != rp {
		return false
	}
	return mediaPathRE.MatchString(candidate.Path)
}

// One text span replaced when relay media references are localized
type mediaReplacement struct {
	start, end int
	text       string
}

// Same relay media URLs in first seen order and the text replacements that remove them
func findRelayMediaRefs(text, relayURL string) ([]string, []mediaReplacement) {
	var urls []string
	var replacements []mediaReplacement
	var markdownSpans [][2]int
	addURL := func(u string) {
		for _, have := range urls {
			if have == u {
				return
			}
		}
		urls = append(urls, u)
	}
	for _, m := range markdownMediaRE.FindAllStringSubmatchIndex(text, -1) {
		u := text[m[4]:m[5]]
		if !isRelayMediaURL(u, relayURL) {
			continue
		}
		markdownSpans = append(markdownSpans, [2]int{m[0], m[1]})
		replacements = append(replacements, mediaReplacement{m[0], m[1], strings.TrimSpace(text[m[2]:m[3]])})
		addURL(u)
	}
	for _, m := range bareMediaRE.FindAllStringIndex(text, -1) {
		inside := false
		for _, span := range markdownSpans {
			if span[0] <= m[0] && m[1] <= span[1] {
				inside = true
				break
			}
		}
		u := text[m[0]:m[1]]
		if inside || !isRelayMediaURL(u, relayURL) {
			continue
		}
		replacements = append(replacements, mediaReplacement{m[0], m[1], ""})
		addURL(u)
	}
	return urls, replacements
}

// Text with the replacements applied from the end, blank runs collapsed and the result stripped
func replaceMediaRefs(text string, replacements []mediaReplacement) string {
	sorted := append([]mediaReplacement(nil), replacements...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].start != sorted[j].start {
			return sorted[i].start > sorted[j].start
		}
		if sorted[i].end != sorted[j].end {
			return sorted[i].end > sorted[j].end
		}
		return sorted[i].text > sorted[j].text
	})
	for _, r := range sorted {
		text = text[:r.start] + r.text + text[r.end:]
	}
	text = trailingSpaceLineRE.ReplaceAllString(text, "\n")
	return strings.TrimSpace(manyNewlinesRE.ReplaceAllString(text, "\n\n"))
}

// Groups of bits regrouped from one width to another, false on leftover bits when padding is off
func convertBits(data []byte, from, to uint, pad bool) ([]int, bool) {
	acc, bits := 0, uint(0)
	maxv := (1 << to) - 1
	var out []int
	for _, v := range data {
		if int(v)>>from != 0 {
			return nil, false
		}
		acc = (acc << from) | int(v)
		bits += from
		for bits >= to {
			bits -= to
			out = append(out, (acc>>bits)&maxv)
		}
	}
	if pad {
		if bits > 0 {
			out = append(out, (acc<<(to-bits))&maxv)
		}
	} else if bits >= from || (acc<<(to-bits))&maxv != 0 {
		return nil, false
	}
	return out, true
}

// npub bech32 encoding of a 64 hex character public key, empty when the hex is not 32 bytes
func hexToNpub(pubkeyHex string) string {
	raw, err := hex.DecodeString(pubkeyHex)
	if err != nil || len(raw) != 32 {
		return ""
	}
	data, ok := convertBits(raw, 8, 5, true)
	if !ok {
		return ""
	}
	values := append(bech32HRPExpand("npub"), data...)
	values = append(values, 0, 0, 0, 0, 0, 0)
	polymod := bech32Polymod(values) ^ 1
	var b strings.Builder
	b.WriteString("npub1")
	for _, d := range data {
		b.WriteByte(bech32Charset[d])
	}
	for i := 0; i < 6; i++ {
		b.WriteByte(bech32Charset[(polymod>>(5*(5-uint(i))))&31])
	}
	return b.String()
}

// Hex public key of an npub, empty for anything that is not a valid npub
func npubToHex(npub string) string {
	lowered := strings.ToLower(strings.TrimSpace(npub))
	pos := strings.LastIndex(lowered, "1")
	if pos < 1 || lowered[:pos] != "npub" || pos+7 > len(lowered) {
		return ""
	}
	data := make([]int, 0, len(lowered)-pos-1)
	for _, c := range lowered[pos+1:] {
		idx := strings.IndexRune(bech32Charset, c)
		if idx < 0 {
			return ""
		}
		data = append(data, idx)
	}
	if bech32Polymod(append(bech32HRPExpand("npub"), data...)) != 1 {
		return ""
	}
	payload := data[:len(data)-6]
	bytes5 := make([]byte, len(payload))
	for i, v := range payload {
		bytes5[i] = byte(v)
	}
	decoded, ok := convertBits(bytes5, 5, 8, false)
	if !ok || len(decoded) != 32 {
		return ""
	}
	out := make([]byte, 32)
	for i, v := range decoded {
		out[i] = byte(v)
	}
	return hex.EncodeToString(out)
}

// User reference of a hex public key or npub as lowercase hex, empty when it is neither
func normalizeUserRef(ref string) string {
	ref = strings.ToLower(strings.TrimSpace(ref))
	if ref == "" {
		return ""
	}
	if strings.HasPrefix(ref, "npub1") {
		return npubToHex(ref)
	}
	if hex64RE.MatchString(ref) {
		return ref
	}
	return ""
}

// Comma separated text split into items, a list passed through, anything else empty
func splitCSV(raw any) []any {
	switch v := raw.(type) {
	case string:
		parts := strings.Split(v, ",")
		out := make([]any, len(parts))
		for i, p := range parts {
			out[i] = p
		}
		return out
	case []any:
		return v
	case []string:
		out := make([]any, len(v))
		for i, p := range v {
			out[i] = p
		}
		return out
	}
	return nil
}

// Set of hex public keys from a CSV or list of hex keys and npubs
func pubkeySet(raw any) map[string]bool {
	out := map[string]bool{}
	for _, entry := range splitCSV(raw) {
		if s, ok := entry.(string); ok {
			if n := normalizeUserRef(s); n != "" {
				out[n] = true
			}
		}
	}
	return out
}

// Lowercased public key appended once, empty values skipped
func addPubkey(bucket []string, raw any) []string {
	pk := ""
	if raw != nil {
		pk = strings.ToLower(fmt.Sprint(raw))
	}
	if pk == "" {
		return bucket
	}
	for _, have := range bucket {
		if have == pk {
			return bucket
		}
	}
	return append(bucket, pk)
}

// Value of a timestamped cache entry younger than the TTL
type ttlEntry[T any] struct {
	at    time.Time
	value T
}

// Cached value of the key when younger than the TTL
func ttlGet[T any](cache map[string]ttlEntry[T], key string, ttl time.Duration) (T, bool) {
	entry, ok := cache[key]
	if !ok || time.Since(entry.at) >= ttl {
		var zero T
		return zero, false
	}
	return entry.value, true
}

// buzz binary from the configured path, then PATH, then the home bin dir, empty when none exists
func resolveCLIPath(configured string) string {
	if configured != "" {
		p := pypath.Expanduser(configured)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p
		}
		return ""
	}
	if found, ok := pyshutil.Which("buzz", nil); ok {
		return found
	}
	fallback := filepath.Join(pypath.Expanduser("~"), "bin", "buzz")
	if st, err := os.Stat(fallback); err == nil && st.Mode().IsRegular() {
		return fallback
	}
	return ""
}

// Result of one buzz CLI run
type cliResult struct {
	code   int
	stdout string
	stderr string
}

// Runs the buzz CLI with argv only, the key travelling through the child env, timing out with exit 124
func execBuzz(ctx context.Context, cliPath string, args []string, relayURL, privateKey, authTag string, input *string) (cliResult, error) {
	env := tools.IcebathSubprocessEnv(&tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)}, false, nil)
	if real, ok := env["ICEBATH_REAL_HOME"]; ok && real != "" {
		env["HOME"] = real
	}
	env["BUZZ_RELAY_URL"] = relayURL
	env["BUZZ_PRIVATE_KEY"] = privateKey
	delete(env, "BUZZ_AUTH_TAG")
	if authTag != "" {
		env["BUZZ_AUTH_TAG"] = authTag
	}
	runCtx, cancel := context.WithTimeout(ctx, cliTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, cliPath, args...)
	cmd.Env = make([]string, 0, len(env))
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if input != nil {
		cmd.Stdin = strings.NewReader(*input)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return cliResult{}, ctx.Err()
	}
	if runCtx.Err() == context.DeadlineExceeded {
		verb := ""
		if len(args) > 0 {
			verb = args[0]
		}
		detail, _ := json.Marshal(map[string]string{"error": "timeout", "message": fmt.Sprintf("buzz %s timed out after %.1fs", verb, cliTimeout.Seconds())})
		return cliResult{code: 124, stderr: string(detail)}, nil
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return cliResult{}, err
	}
	code := 4
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	return cliResult{code: code, stdout: strings.ToValidUTF8(stdout.String(), "�"), stderr: strings.ToValidUTF8(stderr.String(), "�")}, nil
}

// Untrusted CLI detail bounded in length with a redacted path reduced to its basename
func boundedCLIMessage(message, redactPath string) string {
	if redactPath != "" {
		message = strings.ReplaceAll(message, redactPath, filepath.Base(redactPath))
	}
	if utf8.RuneCountInString(message) <= maxCLIMessageChars {
		return message
	}
	return textutil.First(message, maxCLIMessageChars-3) + "..."
}

// Bounded human readable message from the CLI error contract on stderr
func cliErrorMessage(stderr string, code int, redactPath string) string {
	text := strings.TrimSpace(stderr)
	if data, ok := jsonOr(text, nil).(map[string]any); ok {
		detail, _ := data["message"].(string)
		if strings.TrimSpace(detail) != "" {
			label := "error"
			if category, ok := data["error"].(string); ok && strings.TrimSpace(category) != "" {
				label = strings.TrimSpace(category)
			}
			return boundedCLIMessage(fmt.Sprintf("%s: %s (exit %d)", label, strings.TrimSpace(detail), code), redactPath)
		}
	}
	if text == "" {
		text = fmt.Sprintf("buzz CLI failed with exit code %d", code)
	}
	return boundedCLIMessage(text, redactPath)
}

// Event id of a valid buzz send receipt, or the error describing why it is not one
func parseSendReceipt(stdout string) (string, string) {
	data, ok := jsonOr(stdout, nil).(map[string]any)
	if !ok {
		return "", "invalid CLI response"
	}
	if accepted, isBool := data["accepted"].(bool); isBool && !accepted {
		detail, _ := data["message"].(string)
		if strings.TrimSpace(detail) == "" {
			detail = "message was not accepted"
		}
		return "", boundedCLIMessage(strings.TrimSpace(detail), "")
	}
	eventID, _ := data["event_id"].(string)
	if accepted, _ := data["accepted"].(bool); !accepted || strings.TrimSpace(eventID) == "" {
		return "", "invalid CLI response"
	}
	return strings.TrimSpace(eventID), ""
}

// Decoded JSON of CLI stdout, the default when empty or malformed
func jsonOr(text string, def any) any {
	if strings.TrimSpace(text) == "" {
		return def
	}
	var out any
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return def
	}
	if dec.More() {
		return def
	}
	return out
}

// Objects of a CLI stdout JSON array, non objects dropped
func parseJSONList(stdout string) []map[string]any {
	list, _ := jsonOr(stdout, []any{}).([]any)
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// Integer of a decoded JSON value, zero for anything that is not a number or numeric text
func intOf(v any) int64 {
	switch n := v.(type) {
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i
		}
		if f, err := n.Float64(); err == nil {
			return int64(f)
		}
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case string:
		if i, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64); err == nil {
			return i
		}
	}
	return 0
}

// Text of a decoded JSON value, empty for nil
func strOf(v any) string {
	switch s := v.(type) {
	case nil:
		return ""
	case string:
		return s
	case json.Number:
		return s.String()
	}
	return fmt.Sprint(v)
}

// Target and marker of every well formed NIP-10 e tag of an event
func eTags(event map[string]any) [][2]string {
	tags, _ := event["tags"].([]any)
	var out [][2]string
	for _, raw := range tags {
		tag, ok := raw.([]any)
		if !ok || len(tag) < 2 || strOf(tag[0]) != "e" {
			continue
		}
		marker := ""
		if len(tag) > 3 {
			marker = strOf(tag[3])
		}
		out = append(out, [2]string{strOf(tag[1]), marker})
	}
	return out
}

// Direct parent id from NIP-10 e tags, the reply marker then the root then the last positional
func eventReplyParentID(event map[string]any) string {
	var replyID, rootID, lastE string
	for _, tag := range eTags(event) {
		target := strings.TrimSpace(tag[0])
		if target == "" {
			continue
		}
		lastE = target
		switch tag[1] {
		case "reply":
			replyID = target
		case "root":
			rootID = target
		}
	}
	for _, id := range []string{replyID, rootID, lastE} {
		if id != "" {
			return id
		}
	}
	return ""
}

// Whether the event carries a p tag equal to the public key, case insensitive
func pTagged(event map[string]any, pubkey string) bool {
	tags, _ := event["tags"].([]any)
	for _, raw := range tags {
		tag, ok := raw.([]any)
		if ok && len(tag) > 1 && strOf(tag[0]) == "p" && strings.ToLower(strOf(tag[1])) == pubkey {
			return true
		}
	}
	return false
}

// NIP-10 thread root of an event, the lone reply parent when a thread started there, empty when top level
func extractThreadRoot(event map[string]any) string {
	var root, reply string
	replySet := false
	for _, tag := range eTags(event) {
		switch strings.ToLower(tag[1]) {
		case "root":
			root = tag[0]
		case "reply":
			reply, replySet = tag[0], true
		case "":
			if !replySet {
				reply, replySet = tag[0], true
			}
		}
	}
	if root != "" {
		return root
	}
	return reply
}

// Message type of a cached attachment kind
func attachmentKindType(kind string) gateway.MessageType {
	switch kind {
	case "image":
		return gateway.MessageTypePhoto
	case "video":
		return gateway.MessageTypeVideo
	case "audio":
		return gateway.MessageTypeAudio
	}
	return gateway.MessageTypeDocument
}
