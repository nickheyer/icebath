package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	loadTextStageShare = 0.85
	loadReconnectDelay = 3 * time.Second
	loadStaleEntryTTL  = 120 * time.Second
	loadSSEReadTimeout = 60 * time.Second
)

var loadEvents = []string{"status_change", "model_status"}

type loadEntry struct {
	stage   string
	value   float64
	percent int
	ts      time.Time
}

// LoadProgress is one model's live load stage, in stage value and composite percent
type LoadProgress struct {
	Stage   string  `json:"stage"`
	Value   float64 `json:"value"`
	Percent int     `json:"percent"`
}

var (
	loadMu             sync.Mutex
	loadWatcherRunning bool
	loadSnapshot       = map[string]*loadEntry{}
	loadEnsureWatcher  = ensureLoadWatcher
	loadEndpoint       = func() (string, string, bool) { return managedRoot(context.Background()) }
	loadMonotonicNow   = time.Now
)

// Python round half even of x times 100 clamped to 0 through 100, an error for NaN or infinity
func loadPct(x float64) (int, error) {
	scaled := x * 100
	if math.IsNaN(scaled) {
		return 0, fmt.Errorf("cannot convert float NaN to integer")
	}
	if math.IsInf(scaled, 0) {
		return 0, fmt.Errorf("cannot convert float infinity to integer")
	}
	return int(max(0, min(100, math.RoundToEven(scaled)))), nil
}

// Maps a stage and its in stage value onto one 0 to 100 range, the text model owning 85 percent
func loadCompositePercent(stages []string, current string, value float64) (int, error) {
	if len(stages) == 0 || !slices.Contains(stages, current) || len(stages) == 1 {
		return loadPct(value)
	}
	extras := 0
	for _, s := range stages {
		if s != "text_model" {
			extras++
		}
	}
	extraShare := 0.0
	if extras > 0 {
		extraShare = (1.0 - loadTextStageShare) / float64(extras)
	}
	offset := 0.0
	for _, stage := range stages {
		share := extraShare
		if stage == "text_model" {
			share = loadTextStageShare
		}
		if stage == current {
			return loadPct(offset + share*value)
		}
		offset += share
	}
	return loadPct(value)
}

// Errors a Python event application raises that the SSE reader skips instead of dropping the stream
type loadSkippableError struct{ msg string }

func (e *loadSkippableError) Error() string { return e.msg }

// Python attribute lookup of get on a decoded JSON value, an error when it is not a dict
func loadDict(v any) (map[string]any, error) {
	if m, ok := v.(map[string]any); ok {
		return m, nil
	}
	return nil, fmt.Errorf("'%s' object has no attribute 'get'", loadPyTypeName(v))
}

func loadPyTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case string:
		return "str"
	case []any:
		return "list"
	case float64:
		return "float"
	case map[string]any:
		return "dict"
	}
	return "int"
}

// Iterates a decoded JSON value the way a Python for loop does, a skippable error when it is not iterable
func loadIterate(v any) ([]any, error) {
	switch t := v.(type) {
	case []any:
		return t, nil
	case string:
		var out []any
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, nil
	case map[string]any:
		var out []any
		for _, k := range loadDictKeys(t) {
			out = append(out, k)
		}
		return out, nil
	}
	return nil, &loadSkippableError{fmt.Sprintf("'%s' object is not iterable", loadPyTypeName(v))}
}

// Keys of an object built in Go sorted, the SSE reader substitutes document order before applying
func loadDictKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// Python float of an int or float JSON value, ok false for any other type
func loadNumber(v any) (float64, bool, error) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, true, nil
		}
		return 0, true, nil
	case int:
		return float64(t), true, nil
	case int64:
		return float64(t), true, nil
	case float64:
		return t, true, nil
	case json.Number:
		if n, ok := new(big.Int).SetString(t.String(), 10); ok {
			f, _ := new(big.Float).SetInt(n).Float64()
			if math.IsInf(f, 0) {
				return 0, true, fmt.Errorf("int too large to convert to float")
			}
			return f, true, nil
		}
		f, err := t.Float64()
		return f, err == nil, nil
	}
	return 0, false, nil
}

// Records a loading event's stage and percent or drops the model on any terminal status
func applyLoadEvent(model, event string, data map[string]any) error {
	loadMu.Lock()
	defer loadMu.Unlock()
	raw, has := data["status"]
	status := ""
	if has {
		status = pyStr(raw)
	}
	if slices.Contains(loadEvents, event) && status == "loading" {
		progress := map[string]any{}
		if pyTruthy(data["progress"]) {
			p, err := loadDict(data["progress"])
			if err != nil {
				return err
			}
			progress = p
		}
		var stages []string
		if pyTruthy(progress["stages"]) {
			items, err := loadIterate(progress["stages"])
			if err != nil {
				return err
			}
			for _, s := range items {
				stages = append(stages, pyStr(s))
			}
		}
		current := ""
		if c, ok := progress["current"]; ok {
			current = pyStr(c)
		}
		value, numeric, err := loadNumber(progress["value"])
		entry, ok := loadSnapshot[model]
		if !ok {
			entry = &loadEntry{}
			loadSnapshot[model] = entry
		}
		entry.ts = loadMonotonicNow()
		if current != "" && numeric {
			entry.stage = current
			if err != nil {
				return err
			}
			entry.value = value
			percent, err := loadCompositePercent(stages, current, value)
			if err != nil {
				return err
			}
			entry.percent = percent
		}
		return nil
	}
	if (slices.Contains(loadEvents, event) || event == "model_remove") && status != "loading" {
		delete(loadSnapshot, model)
	}
	return nil
}

func clearLoadSnapshot() {
	loadMu.Lock()
	defer loadMu.Unlock()
	clear(loadSnapshot)
}

// Decodes UTF-8 replacing each maximal invalid subpart with U+FFFD as Python errors replace does
func loadDecodeReplace(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r != utf8.RuneError || size > 1 {
			sb.Write(b[i : i+size])
			i += size
			continue
		}
		sb.WriteRune(utf8.RuneError)
		i += loadMaximalSubpart(b[i:])
	}
	return sb.String()
}

// Length of the maximal invalid UTF-8 subpart at the start of b
func loadMaximalSubpart(b []byte) int {
	c := b[0]
	lo, hi, need := byte(0x80), byte(0xBF), 0
	switch {
	case c >= 0xC2 && c <= 0xDF:
		need = 1
	case c == 0xE0:
		lo, need = 0xA0, 2
	case c >= 0xE1 && c <= 0xEC, c == 0xEE, c == 0xEF:
		need = 2
	case c == 0xED:
		hi, need = 0x9F, 2
	case c == 0xF0:
		lo, need = 0x90, 3
	case c >= 0xF1 && c <= 0xF3:
		need = 3
	case c == 0xF4:
		hi, need = 0x8F, 3
	default:
		return 1
	}
	n := 1
	for k := 0; k < need && n < len(b); k++ {
		if b[n] < lo || b[n] > hi {
			break
		}
		lo, hi = 0x80, 0xBF
		n++
	}
	return n
}

// Applies one SSE line, skipping undecodable data and returning errors that end the stream
func applyLoadSSELine(line []byte) error {
	text := pyStripSpace(loadDecodeReplace(line))
	if !strings.HasPrefix(text, "data:") {
		return nil
	}
	decoded, ok := moaJSONLoads(pyStripSpace(text[5:]))
	if !ok {
		return nil
	}
	msg, err := loadDict(decoded)
	if err != nil {
		return err
	}
	model, event := "", ""
	if v, ok := msg["model"]; ok {
		model = pyStr(v)
	}
	if v, ok := msg["event"]; ok {
		event = pyStr(v)
	}
	data := map[string]any{}
	if pyTruthy(msg["data"]) {
		if data, err = loadDict(msg["data"]); err != nil {
			return err
		}
		data = loadOrderStages(data, pyStripSpace(text[5:]))
	}
	if err := applyLoadEvent(model, event, data); err != nil {
		if _, skip := err.(*loadSkippableError); skip {
			return nil
		}
		return err
	}
	return nil
}

// Reads the router's /models/sse stream line by line until it ends, errors or stays silent for 60 seconds
func streamLoadEvents(base, key string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	idle := time.AfterFunc(loadSSEReadTimeout, cancel)
	defer idle.Stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models/sse", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("HTTP Error %d: %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	var buf []byte
	chunk := make([]byte, 4096)
	for {
		idle.Reset(loadSSEReadTimeout)
		n, err := resp.Body.Read(chunk)
		buf = append(buf, chunk[:n]...)
		for {
			i := bytes.IndexByte(buf, '\n')
			if i < 0 {
				break
			}
			line := buf[:i]
			buf = buf[i+1:]
			if lineErr := applyLoadSSELine(line); lineErr != nil {
				return lineErr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// Follows the managed router's load events forever, clearing the snapshot whenever no stream is live
func watchLoadProgress() {
	defer func() {
		loadMu.Lock()
		loadWatcherRunning = false
		loadMu.Unlock()
	}()
	for {
		base, key, ok := loadEndpoint()
		if !ok {
			clearLoadSnapshot()
			time.Sleep(loadReconnectDelay)
			continue
		}
		if err := streamLoadEvents(base, key); err != nil {
			slog.Debug(fmt.Sprintf("load-progress SSE reconnecting: %v", err))
		}
		clearLoadSnapshot()
		time.Sleep(loadReconnectDelay)
	}
}

func ensureLoadWatcher() {
	loadMu.Lock()
	defer loadMu.Unlock()
	if !loadWatcherRunning {
		loadWatcherRunning = true
		go watchLoadProgress()
	}
}

// Models loading right now with stage, value and percent, starting the watcher on first use
func GetLoadingProgress() map[string]LoadProgress {
	loadEnsureWatcher()
	now := loadMonotonicNow()
	loadMu.Lock()
	defer loadMu.Unlock()
	out := map[string]LoadProgress{}
	for model, e := range loadSnapshot {
		if now.Sub(e.ts) < loadStaleEntryTTL {
			out[model] = LoadProgress{Stage: e.stage, Value: e.value, Percent: e.percent}
		}
	}
	return out
}

// Prompt tokens processed by the busiest processing slot for model, ok false when idle, unreachable or foreign
func GetPrefillProgress(model string) (processed int64, ok bool, err error) {
	base, key, found := loadEndpoint()
	if !found {
		return 0, false, nil
	}
	slots, err := managedGetJSON(context.Background(), base, key, "/slots?"+childQuery(model), 2*time.Second)
	if err != nil {
		return 0, false, nil
	}
	list, _ := slots.([]any)
	var best int64
	for _, slot := range list {
		m, err := loadDict(slot)
		if err != nil {
			return 0, false, err
		}
		if !pyTruthy(m["is_processing"]) {
			continue
		}
		n, valid, err := loadSlotInt(m["n_prompt_tokens_processed"])
		if err != nil {
			return 0, false, err
		}
		if valid {
			best = max(best, n)
		}
	}
	if best > 0 {
		return best, true, nil
	}
	return 0, false, nil
}

// Python int of a slot counter or zero when falsy, valid false where int raises TypeError or ValueError
func loadSlotInt(v any) (int64, bool, error) {
	if !pyTruthy(v) {
		return 0, true, nil
	}
	switch t := v.(type) {
	case bool:
		return 1, true, nil
	case int:
		return int64(t), true, nil
	case int64:
		return t, true, nil
	case float64:
		if math.IsNaN(t) {
			return 0, false, nil
		}
		if math.IsInf(t, 0) {
			return 0, false, fmt.Errorf("cannot convert float infinity to integer")
		}
		return loadSaturate(new(big.Float).SetFloat64(math.Trunc(t))), true, nil
	case json.Number:
		if n, ok := new(big.Int).SetString(t.String(), 10); ok {
			return loadSaturate(new(big.Float).SetInt(n)), true, nil
		}
		f, err := t.Float64()
		if err != nil {
			return 0, false, nil
		}
		return loadSlotInt(f)
	case string:
		n, ok := pythonIntText(t)
		return int64(n), ok, nil
	}
	return 0, false, nil
}

// Integer value of f within the int64 range
func loadSaturate(f *big.Float) int64 {
	if f.Cmp(big.NewFloat(math.MaxInt64)) >= 0 {
		return math.MaxInt64
	}
	if f.Cmp(big.NewFloat(math.MinInt64)) <= 0 {
		return math.MinInt64
	}
	n, _ := f.Int64()
	return n
}

// Copy of data whose object valued progress stages becomes its keys in document order as Python iterates them
func loadOrderStages(data map[string]any, text string) map[string]any {
	progress, ok := data["progress"].(map[string]any)
	if !ok {
		return data
	}
	if _, isObject := progress["stages"].(map[string]any); !isObject {
		return data
	}
	var clean strings.Builder
	inString, escaped := false, false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if inString {
			clean.WriteByte(c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		switch {
		case c == '"':
			inString = true
			clean.WriteByte(c)
		case strings.HasPrefix(text[i:], "NaN"):
			clean.WriteString("null")
			i += len("NaN") - 1
		case strings.HasPrefix(text[i:], "-Infinity"):
			clean.WriteString("null")
			i += len("-Infinity") - 1
		case strings.HasPrefix(text[i:], "Infinity"):
			clean.WriteString("null")
			i += len("Infinity") - 1
		default:
			clean.WriteByte(c)
		}
	}
	dec := json.NewDecoder(strings.NewReader(clean.String()))
	root, err := loadOrderedValue(dec)
	if err != nil {
		return data
	}
	keys := loadLastMember(loadLastMember(loadLastMember(root, "data"), "progress"), "stages")
	members, ok := keys.([]loadMember)
	if !ok {
		return data
	}
	var order []any
	seen := map[string]bool{}
	for _, m := range members {
		if !seen[m.key] {
			seen[m.key] = true
			order = append(order, m.key)
		}
	}
	outProgress := maps.Clone(progress)
	outProgress["stages"] = order
	out := maps.Clone(data)
	out["progress"] = outProgress
	return out
}

type loadMember struct {
	key   string
	value any
}

// Decodes one JSON value keeping objects as ordered member lists
func loadOrderedValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch tok {
	case json.Delim('{'):
		var members []loadMember
		for dec.More() {
			k, err := dec.Token()
			if err != nil {
				return nil, err
			}
			v, err := loadOrderedValue(dec)
			if err != nil {
				return nil, err
			}
			members = append(members, loadMember{key: fmt.Sprint(k), value: v})
		}
		_, err := dec.Token()
		return members, err
	case json.Delim('['):
		for dec.More() {
			if _, err := loadOrderedValue(dec); err != nil {
				return nil, err
			}
		}
		_, err := dec.Token()
		return nil, err
	}
	return tok, nil
}

// Value of the last member named key, as a Python dict keeps the last duplicate
func loadLastMember(v any, key string) any {
	members, _ := v.([]loadMember)
	var found any
	for _, m := range members {
		if m.key == key {
			found = m.value
		}
	}
	return found
}
