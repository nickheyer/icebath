package pydatetime

import "time"

// Wall clock time floored to the microsecond with no monotonic reading, as datetime.now() reads it
func Now() time.Time { return time.Now().Truncate(time.Microsecond) }
