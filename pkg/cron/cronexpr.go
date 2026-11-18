package cron

import (
	"fmt"
	"sort"
	"time"
)

// Years croniter searches either side of the start before it gives up
const croniterMaxYears = 50

// CronExpr is an expression expanded once by the croniter rules, iterated from a wall clock or an instant
type CronExpr struct {
	spec *croniterSpec
}

// ParseCron expands an expression the way croniter constructs an iterator, failing with its message
func ParseCron(expr string) (*CronExpr, error) {
	spec, err := croniterExpand(expr)
	if err != nil {
		return nil, err
	}
	return &CronExpr{spec: spec}, nil
}

// Python datetime, wall clock fields held in UTC plus the fold of an aware value
type pyDT struct {
	wall time.Time
	fold int
}

// Search state of one calc run, the zone nil for a naive start
type cronCalc struct {
	spec    *croniterSpec
	loc     *time.Location
	fields  []cronField
	nth     map[int][]cronNth
	isPrev  bool
	seconds bool
}

func daysInMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

func microTrunc(t time.Time) time.Time { return t.Truncate(time.Microsecond) }

// Wall clock plus a timedelta, OverflowError text outside the datetime range
func pyAddDelta(t time.Time, d time.Duration) (time.Time, error) {
	out := t.Add(d)
	if out.Year() < 1 || out.Year() > 9999 {
		return time.Time{}, fmt.Errorf("date value out of range")
	}
	return out, nil
}

// Absolute relativedelta fields, a negative value leaving the field as it is
type rdAbs struct{ year, month, day, hour, minute, second int }

var rdKeep = rdAbs{-1, -1, -1, -1, -1, -1}

// relativedelta addition, absolute fields and year month shifts first with the day clipped, then the delta
func rdApply(t time.Time, years, months int, abs rdAbs, delta time.Duration) (time.Time, error) {
	if months > 11 || months < -11 {
		s := 1
		if months < 0 {
			s = -1
		}
		div, mod := (months*s)/12, (months*s)%12
		months, years = mod*s, years+div*s
	}
	year := t.Year()
	if abs.year >= 0 {
		year = abs.year
	}
	year += years
	month := int(t.Month())
	if abs.month > 0 {
		month = abs.month
	}
	if months != 0 {
		month += months
		if month > 12 {
			year++
			month -= 12
		} else if month < 1 {
			year--
			month += 12
		}
	}
	if year < 1 || year > 9999 {
		return time.Time{}, fmt.Errorf("year %d is out of range", year)
	}
	day := t.Day()
	if abs.day > 0 {
		day = abs.day
	}
	day = min(day, daysInMonth(year, time.Month(month)))
	hour, minute, second := t.Hour(), t.Minute(), t.Second()
	if abs.hour >= 0 {
		hour = abs.hour
	}
	if abs.minute >= 0 {
		minute = abs.minute
	}
	if abs.second >= 0 {
		second = abs.second
	}
	out := time.Date(year, time.Month(month), day, hour, minute, second, t.Nanosecond(), time.UTC)
	return pyAddDelta(out, delta)
}

func hms(h, m, s int) rdAbs { return rdAbs{-1, -1, -1, h, m, s} }

// Distance from x to the next allowed value, wrapping by the field span, false when the year field has none
func nextNearestDiff(x int, check cronField, rangeVal int, wraps bool) (int, bool) {
	for _, v := range check {
		d := v.n
		if wraps {
			if v.kind == 'l' {
				d = rangeVal
			} else if d > rangeVal {
				continue
			}
		}
		if d >= x {
			return d - x, true
		}
	}
	if !wraps {
		return 0, false
	}
	return check[0].n - x + rangeVal, true
}

// Distance from x back to the previous allowed value, wrapping by the field span, false when the year field has none
func prevNearestDiff(x int, check cronField, rangeVal int, wraps bool) (int, bool) {
	for i := len(check) - 1; i >= 0; i-- {
		if v := check[i]; v.kind != 'l' && v.n <= x {
			return v.n - x, true
		}
	}
	if check.hasLast() {
		return -x, true
	}
	if !wraps {
		return 0, false
	}
	candidate := check[len(check)-1].n
	for i := len(check) - 1; i >= 0; i-- {
		if check[i].n <= rangeVal {
			candidate = check[i].n
			break
		}
	}
	if candidate > rangeVal {
		return -rangeVal, true
	}
	return candidate - x - rangeVal, true
}

func (c *cronCalc) nearestDiff(x int, check cronField, rangeVal int, wraps bool) (int, bool) {
	if c.isPrev {
		return prevNearestDiff(x, check, rangeVal, wraps)
	}
	return nextNearestDiff(x, check, rangeVal, wraps)
}

// Days of the month falling on a cron weekday, the last one being the final element
func nthWeekdaysOfMonth(year int, month time.Month, dow int) []int {
	var out []int
	for d := 1; d <= daysInMonth(year, month); d++ {
		if int(time.Date(year, month, d, 0, 0, 0, 0, time.UTC).Weekday()) == dow {
			out = append(out, d)
		}
	}
	return out
}

// Weekday closest to a day of the month without leaving the month
func nearestWeekday(year int, month time.Month, day int) int {
	last := daysInMonth(year, month)
	day = min(day, last)
	switch time.Date(year, month, day, 0, 0, 0, 0, time.UTC).Weekday() {
	case time.Saturday:
		if day > 1 {
			return day - 1
		}
		return day + 2
	case time.Sunday:
		if day < last {
			return day + 1
		}
		return day - 2
	}
	return day
}

// Step of the day of week nth or nearest weekday walk toward the chosen candidate day
func (c *cronCalc) dayCandidates(d time.Time, year int, month time.Month, candidates []int) (bool, time.Time, error) {
	var keep []int
	for _, cand := range candidates {
		if (c.isPrev && cand <= d.Day()) || (!c.isPrev && d.Day() <= cand) {
			keep = append(keep, cand)
		}
	}
	if len(keep) == 0 {
		if c.isPrev {
			out, err := rdApply(d, 0, 0, hms(23, 59, 59), -time.Duration(d.Day())*24*time.Hour)
			return true, out, err
		}
		days := daysInMonth(year, month)
		out, err := rdApply(d, 0, 0, hms(0, 0, 0), time.Duration(days-d.Day()+1)*24*time.Hour)
		return true, out, err
	}
	sort.Ints(keep)
	target := keep[0]
	if c.isPrev {
		target = keep[len(keep)-1]
	}
	diff := target - d.Day()
	if diff == 0 {
		return false, d, nil
	}
	if c.isPrev {
		out, err := rdApply(d, 0, 0, hms(23, 59, 59), time.Duration(diff)*24*time.Hour)
		return true, out, err
	}
	out, err := rdApply(d, 0, 0, hms(0, 0, 0), time.Duration(diff)*24*time.Hour)
	return true, out, err
}

// One croniter proc, changed true restarting the walk and stop ending it without a match
func (c *cronCalc) proc(i int, d time.Time, year int, month time.Month) (changed, stop bool, out time.Time, err error) {
	f := c.fields
	day := 24 * time.Hour
	switch i {
	case 0:
		if len(f) != 7 || f[cronYearField].star() {
			return false, false, d, nil
		}
		diff, ok := c.nearestDiff(d.Year(), f[cronYearField], 0, false)
		if !ok {
			return false, true, d, nil
		}
		if diff == 0 {
			return false, false, d, nil
		}
		if c.isPrev {
			out, err = rdApply(d, diff, 0, rdAbs{-1, 12, 31, 23, 59, 59}, 0)
		} else {
			out, err = rdApply(d, diff, 0, rdAbs{-1, 1, 1, 0, 0, 0}, 0)
		}
		return true, false, out, err
	case 1:
		if f[cronMonthField].star() {
			return false, false, d, nil
		}
		diff, _ := c.nearestDiff(int(d.Month()), f[cronMonthField], 12, true)
		if diff == 0 {
			return false, false, d, nil
		}
		if c.isPrev {
			if out, err = rdApply(d, 0, diff, rdKeep, 0); err != nil {
				return true, false, out, err
			}
			out, err = rdApply(out, 0, 0, rdAbs{-1, -1, daysInMonth(out.Year(), out.Month()), 23, 59, 59}, 0)
			return true, false, out, err
		}
		out, err = rdApply(d, 0, diff, rdAbs{-1, -1, 1, 0, 0, 0}, 0)
		return true, false, out, err
	case 2:
		if len(c.spec.nearest) > 0 {
			candidates := make([]int, 0, len(c.spec.nearest))
			for _, w := range c.spec.nearest {
				candidates = append(candidates, nearestWeekday(d.Year(), d.Month(), w))
			}
			changed, out, err = c.dayCandidates(d, year, month, candidates)
			return changed, false, out, err
		}
		if f[cronDayField].star() {
			return false, false, d, nil
		}
		days := daysInMonth(year, month)
		if f[cronDayField].hasLast() && days == d.Day() {
			return false, false, d, nil
		}
		span := days
		if c.isPrev {
			prevMonth := (int(month)-2+12)%12 + 1
			prevYear := year
			if month == time.January {
				prevYear--
			}
			span = daysInMonth(prevYear, time.Month(prevMonth))
		}
		diff, _ := c.nearestDiff(d.Day(), f[cronDayField], span, true)
		if diff == 0 {
			return false, false, d, nil
		}
		if c.isPrev {
			out, err = rdApply(d, 0, 0, hms(23, 59, 59), time.Duration(diff)*day)
		} else {
			out, err = rdApply(d, 0, 0, hms(0, 0, 0), time.Duration(diff)*day)
		}
		return true, false, out, err
	case 3:
		if len(c.nth) > 0 {
			var candidates []int
			for wday, nths := range c.nth {
				days := nthWeekdaysOfMonth(d.Year(), d.Month(), wday)
				for _, n := range nths {
					switch {
					case n.last():
						candidates = append(candidates, days[len(days)-1])
					case len(days) >= n.n:
						candidates = append(candidates, days[n.n-1])
					}
				}
			}
			changed, out, err = c.dayCandidates(d, year, month, candidates)
			return changed, false, out, err
		}
		if f[cronDowField].star() {
			return false, false, d, nil
		}
		diff, _ := c.nearestDiff(int(d.Weekday()), f[cronDowField], 7, true)
		if diff == 0 {
			return false, false, d, nil
		}
		if c.isPrev {
			out, err = rdApply(d, 0, 0, hms(23, 59, 59), time.Duration(diff)*day)
		} else {
			out, err = rdApply(d, 0, 0, hms(0, 0, 0), time.Duration(diff)*day)
		}
		return true, false, out, err
	case 4:
		if f[cronHourField].star() {
			return false, false, d, nil
		}
		diff, _ := c.nearestDiff(d.Hour(), f[cronHourField], 24, true)
		if diff == 0 {
			return false, false, d, nil
		}
		if c.isPrev {
			out, err = rdApply(d, 0, 0, hms(-1, 59, 59), time.Duration(diff)*time.Hour)
		} else {
			out, err = rdApply(d, 0, 0, hms(-1, 0, 0), time.Duration(diff)*time.Hour)
		}
		return true, false, out, err
	case 5:
		if f[cronMinuteField].star() {
			return false, false, d, nil
		}
		diff, _ := c.nearestDiff(d.Minute(), f[cronMinuteField], 60, true)
		if diff == 0 {
			return false, false, d, nil
		}
		if c.isPrev {
			out, err = rdApply(d, 0, 0, hms(-1, -1, 59), time.Duration(diff)*time.Minute)
		} else {
			out, err = rdApply(d, 0, 0, hms(-1, -1, 0), time.Duration(diff)*time.Minute)
		}
		return true, false, out, err
	}
	if !c.seconds {
		out, err = rdApply(d, 0, 0, hms(-1, -1, 0), 0)
		return false, false, out, err
	}
	if f[cronSecondField].star() {
		return false, false, d, nil
	}
	diff, _ := c.nearestDiff(d.Second(), f[cronSecondField], 60, true)
	if diff == 0 {
		return false, false, d, nil
	}
	out, err = pyAddDelta(d, time.Duration(diff)*time.Second)
	return true, false, out, err
}

func (c *cronCalc) badDate() error {
	if c.isPrev {
		return &CroniterError{Msg: "failed to find prev date"}
	}
	return &CroniterError{Msg: "failed to find next date"}
}

// Wall clock and fold of an instant in loc, the fold marking the second pass of a repeated hour
func awareOf(inst time.Time, loc *time.Location) pyDT {
	wall := wallOf(inst.In(loc))
	f0, _ := wallInZone(wall, loc)
	if f0.Equal(inst) {
		return pyDT{wall: wall}
	}
	return pyDT{wall: wall, fold: 1}
}

// Instant of an aware wall clock and fold in loc
func instantOf(d pyDT, loc *time.Location) time.Time {
	f0, f1 := wallInZone(d.wall, loc)
	if d.fold == 1 {
		return f1
	}
	return f0
}

func utcOffset(d pyDT, loc *time.Location) time.Duration { return d.wall.Sub(instantOf(d, loc)) }

// Whether the wall clock survives a round trip through UTC in loc
func wallExists(d pyDT, loc *time.Location) bool {
	return wallOf(instantOf(d, loc).In(loc)).Equal(d.wall)
}

func (c *cronCalc) successor(a, b pyDT) bool {
	ai, bi := instantOf(a, c.loc), instantOf(b, c.loc)
	if c.isPrev {
		return ai.Before(bi)
	}
	return ai.After(bi)
}

// Attaches the zone to a found wall clock, the closer resolution of a repeated hour and the next existing minute of a gap
func (c *cronCalc) addTZ(wall time.Time, prev pyDT) (pyDT, bool) {
	fold := 0
	if c.isPrev {
		fold = 1
	}
	result := pyDT{wall: wall, fold: fold}
	if !wallExists(result, c.loc) {
		for !wallExists(result, c.loc) {
			result = pyDT{wall: result.wall.Add(time.Minute)}
		}
		return result, false
	}
	farther := pyDT{wall: wall, fold: 1 - fold}
	if utcOffset(result, c.loc) != utcOffset(farther, c.loc) && !c.successor(result, prev) {
		result = farther
	}
	return result, true
}

// Walk of the croniter procs from now, the zone handling of an aware start applied to the match
func (c *cronCalc) calc(now pyDT, aware bool) (pyDT, error) {
	var offset time.Duration
	switch {
	case c.isPrev:
		offset = -time.Microsecond
	case c.seconds:
		offset = time.Second
	default:
		offset = time.Minute
	}
	unaware, err := pyAddDelta(now.wall, offset)
	if err != nil {
		return pyDT{}, err
	}
	if c.seconds {
		unaware = unaware.Truncate(time.Second)
	} else {
		unaware = unaware.Truncate(time.Minute)
	}
	year, month := unaware.Year(), unaware.Month()
	currentYear := year
	for absInt(year-currentYear) <= croniterMaxYears {
		restart, stop := false, false
		for i := 0; i < 7; i++ {
			changed, halt, out, err := c.proc(i, unaware, year, month)
			if err != nil {
				return pyDT{}, err
			}
			unaware = out
			if halt {
				stop = true
				break
			}
			if changed {
				year, month = unaware.Year(), unaware.Month()
				restart = true
				break
			}
		}
		if stop {
			break
		}
		if restart {
			continue
		}
		if !aware {
			return pyDT{wall: unaware}, nil
		}
		return c.attachZone(now, unaware)
	}
	return pyDT{}, c.badDate()
}

// Zone resolution of a matched wall clock, skipping gaps and weighing the other offset of a transition
func (c *cronCalc) attachZone(now pyDT, unaware time.Time) (pyDT, error) {
	awareTime, exists := c.addTZ(unaware, now)
	if !exists && (!c.successor(awareTime, now) || c.fields[cronHourField].star()) {
		for !exists {
			found, err := c.calc(pyDT{wall: unaware}, false)
			if err != nil {
				return pyDT{}, err
			}
			unaware = found.wall
			awareTime, exists = c.addTZ(unaware, now)
		}
	}
	delta := utcOffset(awareTime, c.loc) - utcOffset(now, c.loc)
	if delta == 0 {
		return awareTime, nil
	}
	altStart, err := pyAddDelta(now.wall, delta)
	if err != nil {
		return pyDT{}, err
	}
	alt, err := c.calc(pyDT{wall: altStart}, false)
	if err != nil {
		return pyDT{}, err
	}
	altAware, _ := c.addTZ(alt.wall, now)
	if !c.successor(altAware, now) {
		return awareTime, nil
	}
	if c.successor(awareTime, altAware) {
		return altAware, nil
	}
	return awareTime, nil
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// Next or previous match from a start, the day of month and weekday union croniter applies when both are set
func (e *CronExpr) calcNext(now pyDT, loc *time.Location, isPrev bool) (pyDT, error) {
	fields := append([]cronField(nil), e.spec.fields...)
	c := &cronCalc{spec: e.spec, loc: loc, fields: fields, nth: e.spec.nth, isPrev: isPrev, seconds: len(fields) > 5}
	aware := loc != nil
	if !fields[cronDayField].star() && !fields[cronDowField].star() {
		bak := fields[cronDowField]
		fields[cronDowField] = cronField{{kind: '*'}}
		t1, err := c.calc(now, aware)
		if err != nil {
			return pyDT{}, err
		}
		fields[cronDowField] = bak
		fields[cronDayField] = cronField{{kind: '*'}}
		t2, err := c.calc(now, aware)
		if err != nil {
			return pyDT{}, err
		}
		if isPrev {
			if t1.wall.After(t2.wall) {
				return t1, nil
			}
			return t2, nil
		}
		if t1.wall.Before(t2.wall) {
			return t1, nil
		}
		return t2, nil
	}
	return c.calc(now, aware)
}

// Next returns the first match after the wall clock of t, read as a naive datetime and returned in UTC
func (e *CronExpr) Next(t time.Time) (time.Time, error) {
	got, err := e.calcNext(pyDT{wall: microTrunc(wallOf(t))}, nil, false)
	return got.wall, err
}

// Prev returns the last match before the wall clock of t, read as a naive datetime and returned in UTC
func (e *CronExpr) Prev(t time.Time) (time.Time, error) {
	got, err := e.calcNext(pyDT{wall: microTrunc(wallOf(t))}, nil, true)
	return got.wall, err
}

// NextAt returns the first match after instant t with the zone of t attached the way an aware start works
func (e *CronExpr) NextAt(t time.Time) (time.Time, error) {
	loc := t.Location()
	got, err := e.calcNext(awareOf(microTrunc(t), loc), loc, false)
	if err != nil {
		return time.Time{}, err
	}
	return instantOf(got, loc), nil
}

// PrevAt returns the last match before instant t with the zone of t attached the way an aware start works
func (e *CronExpr) PrevAt(t time.Time) (time.Time, error) {
	loc := t.Location()
	got, err := e.calcNext(awareOf(microTrunc(t), loc), loc, true)
	if err != nil {
		return time.Time{}, err
	}
	return instantOf(got, loc), nil
}
