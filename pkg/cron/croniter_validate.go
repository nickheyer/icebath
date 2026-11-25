package cron

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"sort"
	"strconv"
	"strings"

	regexutil "github.com/nickheyer/icebath/pkg/utils/regex"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

const (
	cronMinuteField = 0
	cronHourField   = 1
	cronDayField    = 2
	cronMonthField  = 3
	cronDowField    = 4
	cronSecondField = 5
	cronYearField   = 6
	// Step bound past every field span, range expansions with a larger step yield their first value only
	cronStepCap = 1 << 20
)

var (
	croniterRanges      = [7][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}, {0, 59}, {1970, 2099}}
	croniterLenMeansAll = [7]int{60, 24, 31, 12, 7, 60, 130}
	croniterAliases     = map[string]string{"@midnight": "0 0 * * *", "@hourly": "0 * * * *", "@daily": "0 0 * * *",
		"@weekly": "0 0 * * 0", "@monthly": "0 0 1 * *", "@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *"}
	monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
		"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
	dowNames         = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}
	croniterStepRe   = regexutil.Lazy(regexp.MustCompile, `^([^-]+)-([^-/]+)(/(\d+))?$`)
	croniterOnlyInt  = regexutil.Lazy(regexp.MustCompile, `^\d+$`)
	croniterStarInt  = regexutil.Lazy(regexp.MustCompile, `^(\d+|\*)$`)
	croniterNearestW = regexutil.Lazy(regexp.MustCompile, `^(?:(\d+)w|w(\d+))$`)
	croniterHashRe   = regexutil.Lazy(regexp.MustCompile, `^(?P<hash_type>h|r)(\((?P<range_begin>\d+)-(?P<range_end>\d+)\))?(\/(?P<divisor>\d+))?$`)
	croniterStarStep = regexutil.Lazy(regexp.MustCompile, `^\*(\/.+)$`)
	croniterSlash    = regexutil.Lazy(regexp.MustCompile, `^(.+)\/(.+)$`)
	croniterSpecial  = regexutil.Lazy(regexp.MustCompile, `^(?P<pre>((?P<he>((sun|mon|tue|wed|thu|fri|sat)(-(sun|mon|tue|wed|thu|fri|sat))?)`+
		`|((jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)(-(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec))?)|\w+)#)|l)(?P<last>\d+)$`)
)

// CroniterError reports why a cron expression cannot be parsed
type CroniterError struct{ Msg string }

func (e *CroniterError) Error() string { return e.Msg }

func croniterFail(format string, args ...any) error {
	return &CroniterError{Msg: fmt.Sprintf(format, args...)}
}

// One expanded value, a number, the star wildcard or the last day marker
type cronVal struct {
	kind byte
	n    int
}

// Expanded field values sorted the croniter way, star first and the last day marker after numbers
type cronField []cronVal

func (f cronField) star() bool { return len(f) > 0 && f[0].kind == '*' }

func (f cronField) hasLast() bool {
	for _, v := range f {
		if v.kind == 'l' {
			return true
		}
	}
	return false
}

// Ordinal of a weekday inside its month, zero standing for the last occurrence
type cronNth struct{ n int }

func (n cronNth) last() bool { return n.n == 0 }

// Expansion croniter keeps for an expression, fields plus nth weekday and nearest weekday tables
type croniterSpec struct {
	fields      []cronField
	nthOrder    []int
	nth         map[int][]cronNth
	nearest     []int
	expressions []string
}

func croniterAlpha(field int, key string, expressions []string) (string, error) {
	switch field {
	case cronDayField:
		if key == "l" {
			return "l", nil
		}
	case cronMonthField:
		if v, ok := monthNames[key]; ok {
			return strconv.Itoa(v), nil
		}
	case cronDowField:
		if v, ok := dowNames[key]; ok {
			return strconv.Itoa(v), nil
		}
	}
	return "", croniterFail("[%s] is not acceptable", strings.Join(expressions, " "))
}

// Low alias croniter applies to a value unless the field count refuses it for that field
func croniterValueAlias(val, field, length int) int {
	mapped := -1
	switch {
	case (field == cronDayField || field == cronMonthField) && val == 0:
		mapped = 1
	case field == cronDowField && val == 7:
		mapped = 0
	}
	if mapped < 0 {
		return val
	}
	switch {
	case (field == cronDayField || field == cronMonthField) && length == 5,
		(field == cronMonthField || field == cronDowField) && length == 6,
		(field == cronDayField || field == cronMonthField || field == cronDowField) && length == 7:
		return val
	}
	return mapped
}

// Value of a run of ASCII digits, capped at cronStepCap, which lies past every field range
func cronDigits(s string) int64 {
	n, err := textutil.ParseInt(s)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0
	}
	return min(n, cronStepCap)
}

// Hashed or random field form expanded to plain syntax the way the hash expander does without a hash id
func croniterHashExpand(field int, expr string) (string, error) {
	hashRe := croniterHashRe()
	m := hashRe.FindStringSubmatch(expr)
	if m == nil {
		return expr, nil
	}
	if m[hashRe.SubexpIndex("hash_type")] == "h" {
		return "", croniterFail("Hashed definitions must include hash_id")
	}
	beginText, endText, divText := m[hashRe.SubexpIndex("range_begin")], m[hashRe.SubexpIndex("range_end")], m[hashRe.SubexpIndex("divisor")]
	if beginText != "" && endText != "" && cronDigits(beginText) >= cronDigits(endText) {
		return "", croniterFail("Range end must be greater than range begin")
	}
	random := func(begin, end int64) int64 {
		return begin + int64(uint64(rand.Uint32()>>uint(field))%uint64(end-begin+1))
	}
	switch {
	case beginText != "" && endText != "" && divText != "":
		div := cronDigits(divText)
		if div == 0 {
			return "", croniterFail("Bad expression: %s", expr)
		}
		begin := cronDigits(beginText)
		x := random(begin, begin+div-1)
		return strconv.FormatInt(x, 10) + "-" + strconv.FormatInt(cronDigits(endText), 10) + "/" + strconv.FormatInt(div, 10), nil
	case beginText != "" && endText != "":
		return strconv.FormatInt(random(cronDigits(beginText), cronDigits(endText)), 10), nil
	case divText != "":
		div := cronDigits(divText)
		if div == 0 {
			return "", croniterFail("Bad expression: %s", expr)
		}
		lo := int64(croniterRanges[field][0])
		x := random(lo, lo+div-1)
		return strconv.FormatInt(x, 10) + "-" + strconv.Itoa(croniterRanges[field][1]) + "/" + strconv.FormatInt(div, 10), nil
	}
	return strconv.FormatInt(random(int64(croniterRanges[field][0]), int64(croniterRanges[field][1])), 10), nil
}

// Expands an expression the way croniter does constructing an iterator, raising its exact messages
func croniterExpand(exprFormat string) (*croniterSpec, error) {
	efl := strings.ToLower(exprFormat)
	if alias, ok := croniterAliases[efl]; ok {
		efl = alias
	}
	expressions := strings.Fields(efl)
	n := len(expressions)
	if n < 5 || n > 7 {
		return nil, croniterFail("Exactly 5, 6 or 7 columns has to be specified for iterator expression.")
	}
	spec := &croniterSpec{nth: map[int][]cronNth{}, expressions: expressions}
	for field, token := range expressions {
		expr, err := croniterHashExpand(field, token)
		if err != nil {
			return nil, err
		}
		if strings.Contains(expr, "?") {
			if expr != "?" {
				return nil, croniterFail("[%s] is not acceptable. Question mark can not used with other characters", exprFormat)
			}
			if field != cronDayField && field != cronDowField {
				return nil, croniterFail("[%s] is not acceptable. Question mark can only used in day_of_month or day_of_week", exprFormat)
			}
			expr = "*"
		}
		res, err := croniterExpandField(spec, exprFormat, field, expr)
		if err != nil {
			return nil, err
		}
		if len(res) == croniterLenMeansAll[field] &&
			!(field == cronDayField && !strings.Contains(expressions[cronDowField], "*")) &&
			!(field == cronDowField && !strings.Contains(expressions[cronDayField], "*")) {
			res = cronField{{kind: '*'}}
		}
		spec.fields = append(spec.fields, res)
	}
	if len(spec.nthOrder) > 0 {
		var rest []int
		distinct := 0
		for _, v := range spec.fields[cronDowField] {
			distinct++
			if v.kind != 'n' {
				continue
			}
			if _, isNth := spec.nth[v.n]; !isNth {
				rest = append(rest, v.n)
			}
		}
		if len(rest) > 0 && distinct != croniterLenMeansAll[cronDowField] {
			return nil, &CroniterError{Msg: fmt.Sprintf("day-of-week field does not support mixing literal values and nth"+
				" day of week syntax: cron %q lists the plain days %s beside %s", exprFormat, joinInts(rest), spec.nthText())}
		}
	}
	return spec, nil
}

// Expands one comma list field into its sorted distinct values, recording nth and nearest weekday entries
func croniterExpandField(spec *croniterSpec, exprFormat string, field int, expr string) (cronField, error) {
	lo, hi := croniterRanges[field][0], croniterRanges[field][1]
	length := len(spec.expressions)
	pending := strings.Split(expr, ",")
	seen := map[string]bool{}
	var res []cronVal
	for len(pending) > 0 {
		e := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		var nth *cronNth
		if field == cronDowField {
			if g := croniterSpecial().FindStringSubmatch(e); g != nil {
				he, last := g[croniterSpecial().SubexpIndex("he")], g[croniterSpecial().SubexpIndex("last")]
				if he != "" {
					e = he
					k := cronDigits(last)
					if k < 1 || k > 5 {
						return nil, croniterFail("[%s] is not acceptable. Invalid day_of_week value: %d", exprFormat, k)
					}
					nth = &cronNth{n: int(k)}
				} else if last != "" {
					e = last
					nth = &cronNth{}
				}
			}
		}
		if field == cronDayField {
			if w := croniterNearestW().FindStringSubmatch(e); w != nil {
				text := w[1]
				if text == "" {
					text = w[2]
				}
				day := cronDigits(text)
				if day < 1 || day > 31 {
					return nil, croniterFail("[%s] is not acceptable, nearest weekday day value %d out of range", exprFormat, day)
				}
				if len(pending) > 0 || len(res) > 0 {
					return nil, croniterFail("[%s] is not acceptable. \"W\" can only be used with a single day value, not in a list or range", exprFormat)
				}
				spec.nearest = append(spec.nearest, int(day))
				res = append(res, cronVal{kind: 'n', n: int(day)})
				continue
			}
		}
		t := e
		if m := croniterStarStep().FindStringSubmatch(e); m != nil {
			t = fmt.Sprintf("%d-%d%s", lo, hi, m[1])
		}
		m := croniterStepRe().FindStringSubmatch(t)
		if m == nil {
			t = e
			if s := croniterSlash().FindStringSubmatch(e); s != nil {
				t = fmt.Sprintf("%s-%d/%s", s[1], hi, s[2])
			}
			m = croniterStepRe().FindStringSubmatch(t)
		}
		if m != nil {
			rng, err := croniterRange(spec, exprFormat, field, length, m[1], m[2], m[4])
			if err != nil {
				return nil, err
			}
			items := make([]string, len(rng))
			for i, v := range rng {
				items[i] = strconv.Itoa(v)
				if field == cronDowField && nth != nil && !nth.last() {
					items[i] += "#" + strconv.Itoa(nth.n)
				}
			}
			for _, it := range items {
				if !seen[it] {
					pending = append(pending, it)
				}
			}
			for _, it := range items {
				seen[it] = true
			}
			continue
		}
		if strings.HasPrefix(t, "-") {
			return nil, croniterFail("[%s] is not acceptable, negative numbers not allowed", exprFormat)
		}
		if !croniterStarInt().MatchString(t) {
			conv, err := croniterAlpha(field, t, spec.expressions)
			if err != nil {
				return nil, err
			}
			t = conv
		}
		var v cronVal
		switch t {
		case "*":
			v = cronVal{kind: '*'}
		case "l":
			v = cronVal{kind: 'l'}
		default:
			x := croniterValueAlias(int(cronDigits(t)), field, length)
			if x < lo || x > hi {
				return nil, croniterFail("[%s] is not acceptable, out of range", exprFormat)
			}
			v = cronVal{kind: 'n', n: x}
		}
		res = append(res, v)
		if field == cronDowField && nth != nil && v.kind == 'n' {
			spec.addNth(v.n, *nth)
		}
	}
	return sortCronField(res), nil
}

// Values of a low-high/step band with croniter's wrap around for a band whose end precedes its start
func croniterRange(spec *croniterSpec, exprFormat string, field, length int, low, high, stepText string) ([]int, error) {
	lo, hi := croniterRanges[field][0], croniterRanges[field][1]
	if field == cronDayField && high == "l" {
		high = "31"
	}
	var err error
	if !croniterOnlyInt().MatchString(low) {
		if low, err = croniterAlpha(field, low, spec.expressions); err != nil {
			return nil, err
		}
	}
	if !croniterOnlyInt().MatchString(high) {
		if high, err = croniterAlpha(field, high, spec.expressions); err != nil {
			return nil, err
		}
	}
	step := 1
	if stepText != "" {
		s := cronDigits(stepText)
		if s == 0 {
			return nil, croniterFail("[%s] step 0 in field %d is not acceptable", exprFormat, field)
		}
		step = int(s)
	}
	if !croniterOnlyInt().MatchString(low) || !croniterOnlyInt().MatchString(high) {
		return nil, croniterFail("[%s] bands %s-%s in field %d are not acceptable", exprFormat, low, high, field)
	}
	l := croniterValueAlias(int(cronDigits(low)), field, length)
	h := croniterValueAlias(int(cronDigits(high)), field, length)
	if max(l, h) > max(lo, hi) {
		return nil, croniterFail("%s is out of bands", exprFormat)
	}
	var rng []int
	switch {
	case l > h:
		for v := l; v <= hi; v += step {
			rng = append(rng, v)
		}
		skip := 0
		if len(rng) > 0 {
			lastVal := rng[len(rng)-1]
			if lastVal < lo {
				return nil, croniterFail("[%s] range %d-%d in field %d is out of bands", exprFormat, l, h, field)
			}
			alreadySkipped := hi - lastVal
			if lastVal-lo+step > hi-lo+1 && alreadySkipped < step {
				skip = step - alreadySkipped
			}
		}
		for v := lo + skip; v <= h; v += step {
			rng = append(rng, v)
		}
	case l == h:
		for v := lo; v <= hi; v += step {
			rng = append(rng, v)
		}
	default:
		for v := l; v <= h; v += step {
			rng = append(rng, v)
		}
	}
	return rng, nil
}

func (s *croniterSpec) addNth(weekday int, nth cronNth) {
	cur, ok := s.nth[weekday]
	if !ok {
		s.nthOrder = append(s.nthOrder, weekday)
	}
	for _, have := range cur {
		if have == nth {
			return
		}
	}
	s.nth[weekday] = append(cur, nth)
}

// Distinct values sorted by croniter's two digit key, star first and the last day marker last
func sortCronField(res []cronVal) cronField {
	seen := map[cronVal]bool{}
	out := cronField{}
	for _, v := range res {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	rank := func(v cronVal) int {
		switch v.kind {
		case '*':
			return 0
		case 'l':
			return 2
		}
		return 1
	}
	sort.Slice(out, func(a, b int) bool {
		ra, rb := rank(out[a]), rank(out[b])
		if ra != rb {
			return ra < rb
		}
		return out[a].n < out[b].n
	})
	if len(out) == 1 && out[0].kind == '*' {
		return cronField{{kind: '*'}}
	}
	return out
}

// Numbers joined by commas in the order given
func joinInts(vals []int) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, ", ")
}

// Nth weekday entries in the order the expression gave them, spelled as cron writes them: weekday#n, or L and the weekday for the last one in the month
func (s *croniterSpec) nthText() string {
	var parts []string
	for _, wd := range s.nthOrder {
		for _, n := range s.nth[wd] {
			if n.last() {
				parts = append(parts, "L"+strconv.Itoa(wd))
				continue
			}
			parts = append(parts, strconv.Itoa(wd)+"#"+strconv.Itoa(n.n))
		}
	}
	return strings.Join(parts, ", ")
}
