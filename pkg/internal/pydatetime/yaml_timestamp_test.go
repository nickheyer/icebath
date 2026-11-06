package pydatetime

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Timestamp scalars build the date and datetime values with the reprs and strs Python prints
func TestYAMLTimestamp(t *testing.T) {
	for _, c := range []struct{ in, repr, str, typ, err string }{
		{"2001-12-14", "datetime.date(2001, 12, 14)", "2001-12-14", "date", ""},
		{"2001-12-14t21:59:43.10-05:00", "datetime.datetime(2001, 12, 14, 21, 59, 43, 100000, tzinfo=datetime.timezone(datetime.timedelta(days=-1, seconds=68400), '-05:00'))", "2001-12-14 21:59:43.100000-05:00", "datetime", ""},
		{"2001-12-15T02:59:43.1Z", "datetime.datetime(2001, 12, 15, 2, 59, 43, 100000, tzinfo=datetime.timezone(datetime.timedelta(0), 'Z'))", "2001-12-15 02:59:43.100000+00:00", "datetime", ""},
		{"2001-12-14 21:59:43.10 -5", "datetime.datetime(2001, 12, 14, 21, 59, 43, 100000, tzinfo=datetime.timezone(datetime.timedelta(days=-1, seconds=68400), '-5'))", "2001-12-14 21:59:43.100000-05:00", "datetime", ""},
		{"2002-12-14 01:02:00", "datetime.datetime(2002, 12, 14, 1, 2)", "2002-12-14 01:02:00", "datetime", ""},
		{"2002-1-4 01:02:03", "datetime.datetime(2002, 1, 4, 1, 2, 3)", "2002-01-04 01:02:03", "datetime", ""},
		{"2002-12-14 01:02:03.9999996", "datetime.datetime(2002, 12, 14, 1, 2, 3)", "2002-12-14 01:02:03", "datetime", ""},
		{"2002-12-14 01:02:03.0000015+05:30", "datetime.datetime(2002, 12, 14, 1, 2, 3, 2, tzinfo=datetime.timezone(datetime.timedelta(seconds=19800), '+05:30'))", "2002-12-14 01:02:03.000002+05:30", "datetime", ""},
		{"2001-13-01", "", "", "", "month must be in 1..12"},
		{"2001-02-29", "", "", "", "day is out of range for month"},
		{"0000-01-01", "", "", "", "year 0 is out of range"},
		{"2001-01-01 24:00:00", "", "", "", "hour must be in 0..23"},
		{"2001-01-01 23:60:00", "", "", "", "minute must be in 0..59"},
		{"2001-01-01 23:00:61", "", "", "", "second must be in 0..59"},
		{"2001-01-01 23:00:00+24", "", "", "", "offset must be a timedelta strictly between -timedelta(hours=24) and timedelta(hours=24), not datetime.timedelta(days=1)."},
	} {
		v, ok, err := YAMLTimestamp(c.in)
		if !ok {
			t.Fatalf("%q did not resolve as a timestamp", c.in)
		}
		if c.err != "" {
			if e, isErr := err.(*Error); !isErr || e.Kind != "ValueError" || e.Msg != c.err {
				t.Fatalf("%q error = %v", c.in, err)
			}
			continue
		}
		if err != nil || pyrepr.Repr(v) != c.repr || pyrepr.Str(v) != c.str || pyrepr.TypeName(v) != c.typ {
			t.Fatalf("%q = %s / %s / %s %v", c.in, pyrepr.Repr(v), pyrepr.Str(v), pyrepr.TypeName(v), err)
		}
	}
	for _, s := range []string{"2001-1-2", "2001-12-14x", "20011214", "2001-12-14 1:2:3", "'2001-12-14'"} {
		if _, ok, _ := YAMLTimestamp(s); ok {
			t.Fatalf("%q resolved as a timestamp", s)
		}
	}
}
