package cron

import (
	"fmt"
	"math/big"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
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
	dowNames           = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}
	croniterStepRe     = pyre.MustCompile(`^([^-]+)-([^-/]+)(/(\d+))?$`)
	croniterOnlyInt    = pyre.MustCompile(`^\d+$`)
	croniterStarInt    = pyre.MustCompile(`^(\d+|\*)$`)
	croniterNearestW   = pyre.MustCompile(`^(?:(\d+)w|w(\d+))$`)
	croniterHashPat    = `^(?P<hash_type>h|r)(\((?P<range_begin>\d+)-(?P<range_end>\d+)\))?(\/(?P<divisor>\d+))?$`
	croniterHashRe     = pyre.MustCompile(croniterHashPat)
	croniterHashIdx    = mustGroupIndex(croniterHashPat)
	croniterStarStep   = pyre.MustCompile(`^\*(\/.+)$`)
	croniterSlash      = pyre.MustCompile(`^(.+)\/(.+)$`)
	croniterSpecialPat = `^(?P<pre>((?P<he>((sun|mon|tue|wed|thu|fri|sat)(-(sun|mon|tue|wed|thu|fri|sat))?)` +
		`|((jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)(-(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec))?)|\w+)#)|l)(?P<last>\d+)$`
	croniterSpecial    = pyre.MustCompile(croniterSpecialPat)
	croniterSpecialIdx = mustGroupIndex(croniterSpecialPat)
	// Process wide stand in for the randomized str hash of the last weekday marker
	pyLastMarkerHash = sync.OnceValue(rand.Uint64)
)

// CroniterError is the exception croniter raises building an iterator, its str as the message
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

func reFind(re *regexp2.Regexp, s string) *regexp2.Match {
	m, err := re.FindStringMatch(s)
	if err != nil {
		return nil
	}
	return m
}

// Python group name to translated group number table of a pattern
func mustGroupIndex(pattern string) map[string]int {
	idx, err := pyre.GroupIndex(pattern)
	if err != nil {
		panic(err)
	}
	return idx
}

func reGroup(m *regexp2.Match, index map[string]int, name string) string {
	return reNum(m, index[name])
}

func reNum(m *regexp2.Match, n int) string {
	g := m.GroupByNumber(n)
	if g == nil || len(g.Captures) == 0 {
		return ""
	}
	return g.String()
}

// Python int of a digit run, Unicode decimal digits included
func pyDigits(s string) *big.Int {
	n, _ := pystr.IntText(s)
	if n == nil {
		return new(big.Int)
	}
	return n
}

// Small int of a value already known to be in a field range, or a bound past every range
func clampInt(n *big.Int) int {
	if !n.IsInt64() || n.Int64() > cronStepCap {
		return cronStepCap
	}
	return int(n.Int64())
}

// Hashed or random field form expanded to plain syntax the way the hash expander does without a hash id
func croniterHashExpand(field int, expr string) (string, error) {
	m := reFind(croniterHashRe, expr)
	if m == nil {
		return expr, nil
	}
	if reGroup(m, croniterHashIdx, "hash_type") == "h" {
		return "", croniterFail("Hashed definitions must include hash_id")
	}
	beginText, endText, divText := reGroup(m, croniterHashIdx, "range_begin"), reGroup(m, croniterHashIdx, "range_end"), reGroup(m, croniterHashIdx, "divisor")
	if beginText != "" && endText != "" && pyDigits(beginText).Cmp(pyDigits(endText)) >= 0 {
		return "", croniterFail("Range end must be greater than range begin")
	}
	random := func(begin, end *big.Int) *big.Int {
		crc := new(big.Int).SetUint64(uint64(rand.Uint32()) >> uint(field))
		span := new(big.Int).Sub(end, begin)
		span.Add(span, big.NewInt(1))
		crc.Mod(crc, span)
		return crc.Add(crc, begin)
	}
	switch {
	case beginText != "" && endText != "" && divText != "":
		div := pyDigits(divText)
		if div.Sign() == 0 {
			return "", croniterFail("Bad expression: %s", expr)
		}
		begin := pyDigits(beginText)
		end := new(big.Int).Add(div, begin)
		x := random(begin, end.Sub(end, big.NewInt(1)))
		return x.String() + "-" + pyDigits(endText).String() + "/" + div.String(), nil
	case beginText != "" && endText != "":
		return random(pyDigits(beginText), pyDigits(endText)).String(), nil
	case divText != "":
		div := pyDigits(divText)
		if div.Sign() == 0 {
			return "", croniterFail("Bad expression: %s", expr)
		}
		lo := big.NewInt(int64(croniterRanges[field][0]))
		end := new(big.Int).Add(div, lo)
		x := random(lo, end.Sub(end, big.NewInt(1)))
		return x.String() + "-" + strconv.Itoa(croniterRanges[field][1]) + "/" + div.String(), nil
	}
	return random(big.NewInt(int64(croniterRanges[field][0])), big.NewInt(int64(croniterRanges[field][1]))).String(), nil
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
				" day of week syntax.  Cron: '%s'    dow=%s vs nth=%s", exprFormat, pyIntSetRepr(rest), spec.nthRepr())}
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
			if g := reFind(croniterSpecial, e); g != nil {
				he, last := reGroup(g, croniterSpecialIdx, "he"), reGroup(g, croniterSpecialIdx, "last")
				if he != "" {
					e = he
					k := pyDigits(last)
					if k.Cmp(big.NewInt(1)) < 0 || k.Cmp(big.NewInt(5)) > 0 {
						return nil, croniterFail("[%s] is not acceptable. Invalid day_of_week value: '%s'", exprFormat, k.String())
					}
					nth = &cronNth{n: int(k.Int64())}
				} else if last != "" {
					e = last
					nth = &cronNth{}
				}
			}
		}
		if field == cronDayField {
			if w := reFind(croniterNearestW, e); w != nil {
				text := reNum(w, 1)
				if text == "" {
					text = reNum(w, 2)
				}
				day := pyDigits(text)
				if day.Cmp(big.NewInt(1)) < 0 || day.Cmp(big.NewInt(31)) > 0 {
					return nil, croniterFail("[%s] is not acceptable, nearest weekday day value '%s' out of range", exprFormat, day.String())
				}
				if len(pending) > 0 || len(res) > 0 {
					return nil, croniterFail("[%s] is not acceptable. 'W' can only be used with a single day value, not in a list or range", exprFormat)
				}
				spec.nearest = append(spec.nearest, int(day.Int64()))
				res = append(res, cronVal{kind: 'n', n: int(day.Int64())})
				continue
			}
		}
		t := e
		if m := reFind(croniterStarStep, e); m != nil {
			t = fmt.Sprintf("%d-%d%s", lo, hi, reNum(m, 1))
		}
		m := reFind(croniterStepRe, t)
		if m == nil {
			t = e
			if s := reFind(croniterSlash, e); s != nil {
				t = fmt.Sprintf("%s-%d/%s", reNum(s, 1), hi, reNum(s, 2))
			}
			m = reFind(croniterStepRe, t)
		}
		if m != nil {
			rng, err := croniterRange(spec, exprFormat, field, length, reNum(m, 1), reNum(m, 2), reNum(m, 4))
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
		if reFind(croniterStarInt, t) == nil {
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
			num := pyDigits(t)
			if !num.IsInt64() {
				return nil, croniterFail("[%s] is not acceptable, out of range", exprFormat)
			}
			x := croniterValueAlias(int(min(num.Int64(), cronStepCap)), field, length)
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
	if reFind(croniterOnlyInt, low) == nil {
		if low, err = croniterAlpha(field, low, spec.expressions); err != nil {
			return nil, err
		}
	}
	if reFind(croniterOnlyInt, high) == nil {
		if high, err = croniterAlpha(field, high, spec.expressions); err != nil {
			return nil, err
		}
	}
	step := 1
	if stepText != "" {
		s := pyDigits(stepText)
		if s.Sign() == 0 {
			return nil, croniterFail("[%s] step '0' in field %d is not acceptable", exprFormat, field)
		}
		step = clampInt(s)
	}
	if reFind(croniterOnlyInt, low) == nil || reFind(croniterOnlyInt, high) == nil {
		return nil, croniterFail("[%s] bands '%s-%s' in field %d are not acceptable", exprFormat, low, high, field)
	}
	l := croniterValueAlias(clampInt(pyDigits(low)), field, length)
	h := croniterValueAlias(clampInt(pyDigits(high)), field, length)
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
				return nil, croniterFail("list.index(x): x not in list")
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

// Python repr of a set of small ints, CPython slot order for non negative ints being ascending
func pyIntSetRepr(vals []int) string {
	items := make([]pySetItem, 0, len(vals))
	for _, v := range vals {
		items = append(items, pySetItem{hash: uint64(v), text: strconv.Itoa(v)})
	}
	return pySetRepr(items)
}

// Python repr of the weekday to ordinal sets mapping in insertion order
func (s *croniterSpec) nthRepr() string {
	parts := make([]string, 0, len(s.nthOrder))
	for _, wd := range s.nthOrder {
		items := make([]pySetItem, 0, len(s.nth[wd]))
		for _, n := range s.nth[wd] {
			if n.last() {
				items = append(items, pySetItem{hash: pyLastMarkerHash(), text: "'l'"})
				continue
			}
			items = append(items, pySetItem{hash: uint64(n.n), text: strconv.Itoa(n.n)})
		}
		parts = append(parts, strconv.Itoa(wd)+": "+pySetRepr(items))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// Member of an emulated CPython set, its hash and its repr
type pySetItem struct {
	hash uint64
	text string
}

// Repr of a CPython set built by adding items in order, iterating its open addressing table by slot
func pySetRepr(items []pySetItem) string {
	const linearProbes, perturbShift = 9, 5
	table := make([]*pySetItem, 8)
	fill := 0
	insert := func(tbl []*pySetItem, it *pySetItem, clean bool) bool {
		mask := uint64(len(tbl) - 1)
		i := it.hash & mask
		perturb := it.hash
		for {
			probes := 0
			if i+linearProbes <= mask {
				probes = linearProbes
			}
			for j := uint64(0); j <= uint64(probes); j++ {
				slot := tbl[i+j]
				if slot == nil {
					tbl[i+j] = it
					return true
				}
				if !clean && slot.hash == it.hash && slot.text == it.text {
					return false
				}
			}
			perturb >>= perturbShift
			i = (i*5 + 1 + perturb) & mask
		}
	}
	for idx := range items {
		if !insert(table, &items[idx], false) {
			continue
		}
		fill++
		mask := len(table) - 1
		if fill*5 >= mask*3 {
			size := 8
			for size <= fill*4 {
				size <<= 1
			}
			grown := make([]*pySetItem, size)
			for _, it := range table {
				if it != nil {
					insert(grown, it, true)
				}
			}
			table = grown
		}
	}
	var parts []string
	for _, it := range table {
		if it != nil {
			parts = append(parts, it.text)
		}
	}
	if len(parts) == 0 {
		return "set()"
	}
	return "{" + strings.Join(parts, ", ") + "}"
}
