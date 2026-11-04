package cron

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cron expression aliases the parser expands before field parsing
var cronAliases = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

var monthNames = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

var dowNames = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

const (
	cronYearMin = 1970
	cronYearMax = 2099
)

// Set of allowed values for one small numeric field
type fieldSet struct {
	bits uint64
}

func (s *fieldSet) add(v int) { s.bits |= 1 << uint(v) }

func (s fieldSet) has(v int) bool { return s.bits&(1<<uint(v)) != 0 }

// Weekday constraint that names an ordinal or the last occurrence inside a month
type dowSpecial struct {
	weekday int
	nth     int
	last    bool
}

// Parsed cron expression with croniter field order and matching rules
type CronExpr struct {
	Source   string
	Fields   int
	seconds  fieldSet
	minutes  fieldSet
	hours    fieldSet
	days     fieldSet
	months   fieldSet
	dows     fieldSet
	years    [cronYearMax - cronYearMin + 1]bool
	lastDay  bool
	dowSpec  []dowSpecial
	domAny   bool
	dowAny   bool
	hasYears bool
}

// Error of a cron expression that cannot be parsed
type CronParseError struct {
	Expr   string
	Reason string
}

func (e *CronParseError) Error() string {
	return fmt.Sprintf("invalid cron expression %q: %s", e.Expr, e.Reason)
}

func cronErr(expr, format string, args ...any) error {
	return &CronParseError{Expr: expr, Reason: fmt.Sprintf(format, args...)}
}

// ParseCron parses a 5, 6 or 7 field expression, the sixth field being seconds and the seventh years
func ParseCron(expr string) (*CronExpr, error) {
	raw := strings.TrimSpace(expr)
	if alias, ok := cronAliases[strings.ToLower(raw)]; ok {
		raw = alias
	}
	parts := strings.Fields(raw)
	if len(parts) < 5 || len(parts) > 7 {
		return nil, cronErr(expr, "expected 5 to 7 fields, got %d", len(parts))
	}
	e := &CronExpr{Source: expr, Fields: len(parts)}
	var err error
	if e.minutes, err = parseNumericField(expr, parts[0], 0, 59, nil); err != nil {
		return nil, err
	}
	if e.hours, err = parseNumericField(expr, parts[1], 0, 23, nil); err != nil {
		return nil, err
	}
	if err = e.parseDayOfMonth(expr, parts[2]); err != nil {
		return nil, err
	}
	if e.months, err = parseNumericField(expr, parts[3], 1, 12, monthNames); err != nil {
		return nil, err
	}
	if err = e.parseDayOfWeek(expr, parts[4]); err != nil {
		return nil, err
	}
	if len(parts) >= 6 {
		if e.seconds, err = parseNumericField(expr, parts[5], 0, 59, nil); err != nil {
			return nil, err
		}
	} else {
		e.seconds.add(0)
	}
	if len(parts) == 7 {
		if err = e.parseYears(expr, parts[6]); err != nil {
			return nil, err
		}
		e.hasYears = true
	}
	return e, nil
}

// Reports whether a field token means every value
func isWildcard(tok string) bool { return tok == "*" || tok == "?" }

// Resolves a single value token to its number using names when the table allows them
func resolveValue(expr, tok string, lo, hi int, names map[string]int) (int, error) {
	t := strings.ToLower(strings.TrimSpace(tok))
	if t == "" {
		return 0, cronErr(expr, "empty value")
	}
	if names != nil {
		if v, ok := names[t]; ok {
			return v, nil
		}
		if len(t) > 3 {
			if v, ok := names[t[:3]]; ok && strings.HasPrefix(fullName(names, v), t) {
				return v, nil
			}
		}
	}
	v, err := strconv.Atoi(t)
	if err != nil {
		return 0, cronErr(expr, "value %q is not a number or a known name", tok)
	}
	if v < lo || v > hi {
		return 0, cronErr(expr, "value %d out of range %d-%d", v, lo, hi)
	}
	return v, nil
}

var fullMonthNames = []string{"", "january", "february", "march", "april", "may", "june", "july", "august", "september", "october", "november", "december"}
var fullDowNames = []string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}

// Long name of a value for the month or weekday table
func fullName(names map[string]int, v int) string {
	if _, ok := names["jan"]; ok {
		if v >= 1 && v <= 12 {
			return fullMonthNames[v]
		}
		return ""
	}
	if v >= 0 && v <= 6 {
		return fullDowNames[v]
	}
	return ""
}

// Expands one comma separated field of plain values, ranges and steps into a set
func parseNumericField(expr, field string, lo, hi int, names map[string]int) (fieldSet, error) {
	var set fieldSet
	for _, item := range strings.Split(field, ",") {
		if err := expandItem(expr, item, lo, hi, names, &set); err != nil {
			return set, err
		}
	}
	return set, nil
}

// Adds the values of one item to the set, handling wildcards, ranges and steps
func expandItem(expr, item string, lo, hi int, names map[string]int, set *fieldSet) error {
	item = strings.TrimSpace(item)
	if item == "" {
		return cronErr(expr, "empty list element")
	}
	step := 1
	body := item
	if idx := strings.Index(item, "/"); idx >= 0 {
		body = item[:idx]
		s, err := strconv.Atoi(item[idx+1:])
		if err != nil || s < 1 {
			return cronErr(expr, "invalid step %q", item[idx+1:])
		}
		step = s
	}
	start, end := lo, hi
	switch {
	case isWildcard(body):
	case strings.Contains(body, "-"):
		halves := strings.SplitN(body, "-", 2)
		a, err := resolveValue(expr, halves[0], lo, hi, names)
		if err != nil {
			return err
		}
		b, err := resolveValue(expr, halves[1], lo, hi, names)
		if err != nil {
			return err
		}
		if names != nil && isDowTable(names) {
			a, b = a%7, b%7
		}
		if a > b {
			return cronErr(expr, "range %q runs backwards", body)
		}
		start, end = a, b
	default:
		v, err := resolveValue(expr, body, lo, hi, names)
		if err != nil {
			return err
		}
		if names != nil && isDowTable(names) {
			v %= 7
		}
		if !strings.Contains(item, "/") {
			set.add(v)
			return nil
		}
		start = v
	}
	for v := start; v <= end; v += step {
		if names != nil && isDowTable(names) {
			set.add(v % 7)
			continue
		}
		set.add(v)
	}
	return nil
}

func isDowTable(names map[string]int) bool {
	_, ok := names["sun"]
	return ok
}

// Parses the day of month field including the L marker for the last day
func (e *CronExpr) parseDayOfMonth(expr, field string) error {
	if isWildcard(field) {
		e.domAny = true
		for d := 1; d <= 31; d++ {
			e.days.add(d)
		}
		return nil
	}
	for _, item := range strings.Split(field, ",") {
		t := strings.ToLower(strings.TrimSpace(item))
		if t == "l" {
			e.lastDay = true
			continue
		}
		if err := expandItem(expr, item, 1, 31, nil, &e.days); err != nil {
			return err
		}
	}
	return nil
}

// Parses the day of week field including 7 as Sunday, L suffixes and # ordinals
func (e *CronExpr) parseDayOfWeek(expr, field string) error {
	if isWildcard(field) {
		e.dowAny = true
		for d := 0; d <= 6; d++ {
			e.dows.add(d)
		}
		return nil
	}
	for _, item := range strings.Split(field, ",") {
		t := strings.ToLower(strings.TrimSpace(item))
		if t == "" {
			return cronErr(expr, "empty list element")
		}
		if idx := strings.Index(t, "#"); idx >= 0 {
			wd, err := resolveValue(expr, t[:idx], 0, 7, dowNames)
			if err != nil {
				return err
			}
			nth, err := strconv.Atoi(t[idx+1:])
			if err != nil || nth < 1 || nth > 5 {
				return cronErr(expr, "invalid weekday ordinal %q", item)
			}
			e.dowSpec = append(e.dowSpec, dowSpecial{weekday: wd % 7, nth: nth})
			continue
		}
		if strings.HasSuffix(t, "l") && len(t) > 1 {
			wd, err := resolveValue(expr, t[:len(t)-1], 0, 7, dowNames)
			if err != nil {
				return err
			}
			e.dowSpec = append(e.dowSpec, dowSpecial{weekday: wd % 7, last: true})
			continue
		}
		if err := expandItem(expr, item, 0, 7, dowNames, &e.dows); err != nil {
			return err
		}
	}
	return nil
}

// Parses the optional year field
func (e *CronExpr) parseYears(expr, field string) error {
	if isWildcard(field) {
		for i := range e.years {
			e.years[i] = true
		}
		return nil
	}
	for _, item := range strings.Split(field, ",") {
		item = strings.TrimSpace(item)
		step := 1
		body := item
		if idx := strings.Index(item, "/"); idx >= 0 {
			body = item[:idx]
			s, err := strconv.Atoi(item[idx+1:])
			if err != nil || s < 1 {
				return cronErr(expr, "invalid step %q", item[idx+1:])
			}
			step = s
		}
		start, end := cronYearMin, cronYearMax
		switch {
		case isWildcard(body):
		case strings.Contains(body, "-"):
			halves := strings.SplitN(body, "-", 2)
			a, err := resolveValue(expr, halves[0], cronYearMin, cronYearMax, nil)
			if err != nil {
				return err
			}
			b, err := resolveValue(expr, halves[1], cronYearMin, cronYearMax, nil)
			if err != nil {
				return err
			}
			if a > b {
				return cronErr(expr, "range %q runs backwards", body)
			}
			start, end = a, b
		default:
			v, err := resolveValue(expr, body, cronYearMin, cronYearMax, nil)
			if err != nil {
				return err
			}
			if !strings.Contains(item, "/") {
				e.years[v-cronYearMin] = true
				continue
			}
			start = v
		}
		for y := start; y <= end; y += step {
			e.years[y-cronYearMin] = true
		}
	}
	return nil
}

// Reports whether the calendar day of t satisfies the day of month and weekday fields
func (e *CronExpr) dayMatches(t time.Time) bool {
	domHit := e.days.has(t.Day()) && !e.domAny
	if e.lastDay && t.Day() == daysInMonth(t.Year(), t.Month()) {
		domHit = true
	}
	dowHit := e.dowMatches(t)
	switch {
	case e.domAny && e.dowAny:
		return true
	case e.domAny:
		return dowHit
	case e.dowAny:
		return domHit
	}
	return domHit || dowHit
}

// Reports whether the weekday field, including ordinals and last markers, accepts t
func (e *CronExpr) dowMatches(t time.Time) bool {
	wd := int(t.Weekday())
	if !e.dowAny && e.dows.has(wd) {
		return true
	}
	for _, s := range e.dowSpec {
		if s.weekday != wd {
			continue
		}
		if s.last && t.Day()+7 > daysInMonth(t.Year(), t.Month()) {
			return true
		}
		if s.nth > 0 && (t.Day()-1)/7+1 == s.nth {
			return true
		}
	}
	return false
}

func daysInMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// Unit the expression schedules at, one minute or one second
func (e *CronExpr) unit() time.Duration {
	if e.Fields >= 6 {
		return time.Second
	}
	return time.Minute
}

func (e *CronExpr) yearOK(y int) bool {
	if !e.hasYears {
		return true
	}
	if y < cronYearMin || y > cronYearMax {
		return false
	}
	return e.years[y-cronYearMin]
}

// ErrNoOccurrence reports a schedule with no reachable occurrence inside the search bound
var ErrNoOccurrence = errors.New("cron expression has no reachable occurrence")

// Next returns the first wall clock occurrence strictly after t, treating t as a naive wall clock
func (e *CronExpr) Next(t time.Time) (time.Time, error) {
	loc := t.Location()
	unit := e.unit()
	cur := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, loc)
	if unit == time.Minute {
		cur = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, loc)
	}
	cur = cur.Add(unit)
	limit := t.Year() + 130
	for cur.Year() <= limit {
		if !e.yearOK(cur.Year()) {
			cur = time.Date(cur.Year()+1, 1, 1, 0, 0, 0, 0, loc)
			continue
		}
		if !e.months.has(int(cur.Month())) {
			cur = time.Date(cur.Year(), cur.Month()+1, 1, 0, 0, 0, 0, loc)
			continue
		}
		if !e.dayMatches(cur) {
			cur = time.Date(cur.Year(), cur.Month(), cur.Day()+1, 0, 0, 0, 0, loc)
			continue
		}
		if !e.hours.has(cur.Hour()) {
			cur = time.Date(cur.Year(), cur.Month(), cur.Day(), cur.Hour()+1, 0, 0, 0, loc)
			continue
		}
		if !e.minutes.has(cur.Minute()) {
			cur = time.Date(cur.Year(), cur.Month(), cur.Day(), cur.Hour(), cur.Minute()+1, 0, 0, loc)
			continue
		}
		if !e.seconds.has(cur.Second()) {
			cur = cur.Add(time.Second)
			continue
		}
		return cur, nil
	}
	return time.Time{}, ErrNoOccurrence
}

// Prev returns the last wall clock occurrence strictly before t, treating t as a naive wall clock
func (e *CronExpr) Prev(t time.Time) (time.Time, error) {
	loc := t.Location()
	unit := e.unit()
	cur := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, loc)
	if unit == time.Minute {
		cur = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, loc)
	}
	if !cur.Before(t) {
		cur = cur.Add(-unit)
	}
	limit := t.Year() - 130
	for cur.Year() >= limit {
		if !e.yearOK(cur.Year()) {
			cur = time.Date(cur.Year()-1, 12, 31, 23, 59, 59, 0, loc)
			continue
		}
		if !e.months.has(int(cur.Month())) {
			cur = time.Date(cur.Year(), cur.Month(), 1, 0, 0, 0, 0, loc).Add(-time.Second)
			continue
		}
		if !e.dayMatches(cur) {
			cur = time.Date(cur.Year(), cur.Month(), cur.Day(), 0, 0, 0, 0, loc).Add(-time.Second)
			continue
		}
		if !e.hours.has(cur.Hour()) {
			cur = time.Date(cur.Year(), cur.Month(), cur.Day(), cur.Hour(), 0, 0, 0, loc).Add(-time.Second)
			continue
		}
		if !e.minutes.has(cur.Minute()) {
			cur = time.Date(cur.Year(), cur.Month(), cur.Day(), cur.Hour(), cur.Minute(), 0, 0, loc).Add(-time.Second)
			continue
		}
		if !e.seconds.has(cur.Second()) {
			cur = cur.Add(-time.Second)
			continue
		}
		if unit == time.Minute {
			return time.Date(cur.Year(), cur.Month(), cur.Day(), cur.Hour(), cur.Minute(), 0, 0, loc), nil
		}
		return cur, nil
	}
	return time.Time{}, ErrNoOccurrence
}

// Matches reports whether t, read as a naive wall clock, is an occurrence of the expression
func (e *CronExpr) Matches(t time.Time) bool {
	if !e.yearOK(t.Year()) || !e.months.has(int(t.Month())) || !e.dayMatches(t) {
		return false
	}
	if !e.hours.has(t.Hour()) || !e.minutes.has(t.Minute()) {
		return false
	}
	if e.Fields >= 6 {
		return e.seconds.has(t.Second())
	}
	return true
}
