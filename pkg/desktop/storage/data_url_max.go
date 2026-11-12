package storage

import (
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/desktop/hardening"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
)

const (
	// Data URL read cap in MB when nothing valid is stored
	DataURLReadDefaultMaxMB = 16
	// Lowest data URL read cap in MB
	DataURLReadMinMaxMB = 1
	// Highest data URL read cap in MB
	DataURLReadMaxMaxMB = 4096
	dataURLReadMaxFile  = "data-url-read-max.json"
)

var jsDecimal = regexp.MustCompile(`^[+-]?(?:[0-9]+\.?[0-9]*|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

// Number conversion of a string as JavaScript Number does
func jsStringToNumber(s string) float64 {
	t := jsTrim(s)
	switch {
	case t == "":
		return 0
	case t == "Infinity" || t == "+Infinity":
		return math.Inf(1)
	case t == "-Infinity":
		return math.Inf(-1)
	case len(t) > 2 && t[0] == '0' && strings.ContainsRune("xXoObB", rune(t[1])):
		radix := map[byte]int{'x': 16, 'X': 16, 'o': 8, 'O': 8, 'b': 2, 'B': 2}[t[1]]
		return radixFloat(t[2:], radix)
	case jsDecimal.MatchString(t):
		f, _ := strconv.ParseFloat(t, 64)
		return f
	}
	return math.NaN()
}

// Number conversion of a JSON value as JavaScript Number does, present false meaning undefined
func jsToNumber(v any, present bool) float64 {
	if !present {
		return math.NaN()
	}
	switch t := v.(type) {
	case nil:
		return 0
	case bool:
		if t {
			return 1
		}
		return 0
	case float64:
		return t
	case string:
		return jsStringToNumber(t)
	case []any:
		return jsStringToNumber(jsString(t))
	}
	return math.NaN()
}

// Rounds and bounds a data URL read cap in MB, the default for anything not finite
func ClampDataURLReadMaxMB(n float64) int {
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return DataURLReadDefaultMaxMB
	}
	r := math.Floor(n + 0.5)
	return int(math.Min(DataURLReadMaxMaxMB, math.Max(DataURLReadMinMaxMB, r)))
}

// Byte cap of a data URL read for a cap in MB
func DataURLReadMaxBytesFromMB(n float64) int64 {
	return int64(ClampDataURLReadMaxMB(n)) * 1024 * 1024
}

// Persisted data URL read cap of the app user data
type dataURLReadMax struct {
	mu   sync.Mutex
	file string
	mb   int
}

// Loads the stored cap, the default for a missing or malformed file
func loadDataURLReadMax(userData string) *dataURLReadMax {
	d := &dataURLReadMax{file: filepath.Join(userData, dataURLReadMaxFile), mb: DataURLReadDefaultMaxMB}
	raw, err := os.ReadFile(d.file)
	if err != nil {
		return d
	}
	text, _ := pycodecs.DecodeReplace(raw, "utf-8")
	doc, err := jsParse(text)
	if err != nil {
		return d
	}
	if obj, ok := doc.(*Object); ok {
		v, present := obj.Get("maxMb")
		d.mb = ClampDataURLReadMaxMB(jsToNumber(v, present))
		return d
	}
	return d
}

func dataURLReadMaxAnswer(mb int) *Object {
	out := NewObject()
	out.Set("maxMb", float64(mb))
	out.Set("defaultMaxMb", float64(DataURLReadDefaultMaxMB))
	out.Set("maxBytes", float64(DataURLReadMaxBytesFromMB(float64(mb))))
	return out
}

// Answers dataUrlReadMax.get
func (s *Service) DataUrlReadMaxGet() *Object {
	s.dataURLMax.mu.Lock()
	defer s.dataURLMax.mu.Unlock()
	return dataURLReadMaxAnswer(s.dataURLMax.mb)
}

// Answers dataUrlReadMax.set for the JavaScript Number of the renderer value, persisting it to user data
func (s *Service) DataUrlReadMaxSet(numberText string) *Object {
	f, err := strconv.ParseFloat(numberText, 64)
	if err != nil {
		f = math.NaN()
	}
	next := ClampDataURLReadMaxMB(f)
	d := s.dataURLMax
	d.mu.Lock()
	d.mb = next
	d.mu.Unlock()
	body := jsStringify(func() *Object { o := NewObject(); o.Set("maxMb", float64(next)); return o }(), "  ")
	werr := os.MkdirAll(filepath.Dir(d.file), 0o777)
	if werr == nil {
		werr = os.WriteFile(d.file, []byte(body), 0o666)
	}
	if werr != nil {
		s.log.Remember("[data-url-read-max] write failed: " + hardening.NodeErr(werr).Error())
	}
	return dataURLReadMaxAnswer(next)
}
