package config

import (
	"bytes"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
)

// Comparison family of a key, keys of two families raising TypeError in a Python sort
func pyKeyFamily(k any) string {
	switch t := k.(type) {
	case string:
		return "str"
	case bool, float32, float64:
		return "num"
	case []byte:
		return "bytes"
	case pydatetime.Date, *pydatetime.Date:
		return "date"
	case pydatetime.DateTime:
		if t.Zone == nil {
			return "naive"
		}
		return "aware"
	case *pydatetime.DateTime:
		return pyKeyFamily(*t)
	case time.Time:
		switch {
		case t.Location() != time.UTC:
			return "aware"
		case t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 && t.Nanosecond() == 0:
			return "date"
		}
		return "naive"
	case nil:
		return "none"
	}
	if _, ok := keyBigInt(k); ok {
		return "num"
	}
	return fmt.Sprintf("%T", k)
}

func keyBigInt(k any) (*big.Int, bool) {
	switch t := k.(type) {
	case int:
		return big.NewInt(int64(t)), true
	case int64:
		return big.NewInt(t), true
	case int32:
		return big.NewInt(int64(t)), true
	case int16:
		return big.NewInt(int64(t)), true
	case int8:
		return big.NewInt(int64(t)), true
	case uint:
		return new(big.Int).SetUint64(uint64(t)), true
	case uint64:
		return new(big.Int).SetUint64(t), true
	case uint32:
		return big.NewInt(int64(t)), true
	case uint16:
		return big.NewInt(int64(t)), true
	case uint8:
		return big.NewInt(int64(t)), true
	case *big.Int:
		return t, true
	case bool:
		if t {
			return big.NewInt(1), true
		}
		return big.NewInt(0), true
	}
	return nil, false
}

// Python less than between two keys of one family, NaN comparing false both ways
func pyKeyLess(a, b any) bool {
	switch x := a.(type) {
	case string:
		return x < b.(string)
	case []byte:
		return bytes.Compare(x, b.([]byte)) < 0
	}
	if fa, ok := pyFloatOf(a); ok {
		return pyNumLess(fa, b, false)
	}
	if fb, ok := pyFloatOf(b); ok {
		return pyNumLess(fb, a, true)
	}
	if ia, ok := keyBigInt(a); ok {
		ib, _ := keyBigInt(b)
		return ia.Cmp(ib) < 0
	}
	ta, tb := pyTimeKey(a), pyTimeKey(b)
	return ta.Before(tb)
}

func pyFloatOf(k any) (float64, bool) {
	switch t := k.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	}
	return 0, false
}

// Float f against a number other, f on the left unless swapped, exact for big ints
func pyNumLess(f float64, other any, swapped bool) bool {
	if math.IsNaN(f) {
		return false
	}
	var cmp int
	if g, ok := pyFloatOf(other); ok {
		if math.IsNaN(g) {
			return false
		}
		switch {
		case f < g:
			cmp = -1
		case f > g:
			cmp = 1
		}
	} else {
		n, _ := keyBigInt(other)
		switch {
		case math.IsInf(f, 1):
			cmp = 1
		case math.IsInf(f, -1):
			cmp = -1
		default:
			cmp = new(big.Float).SetFloat64(f).Cmp(new(big.Float).SetInt(n))
		}
	}
	if swapped {
		return cmp > 0
	}
	return cmp < 0
}

// Instant ordering of a date or datetime key, the wall clock for naive values and the UTC instant for aware ones
func pyTimeKey(k any) time.Time {
	switch t := k.(type) {
	case pydatetime.Date:
		return time.Date(t.Year, time.Month(t.Month), t.Day, 0, 0, 0, 0, time.UTC)
	case *pydatetime.Date:
		return pyTimeKey(*t)
	case pydatetime.DateTime:
		wall := time.Date(t.Year, time.Month(t.Month), t.Day, t.Hour, t.Minute, t.Second, t.Microsecond*1000, time.UTC)
		if t.Zone != nil {
			wall = wall.Add(-t.Zone.Offset)
		}
		return wall
	case *pydatetime.DateTime:
		return pyTimeKey(*t)
	case time.Time:
		return t
	}
	return time.Time{}
}

// Order a Python sorted call gives keys, the input order when two keys cannot be compared
func pySortedOrder(keys []any) []int {
	idx := make([]int, len(keys))
	for i := range idx {
		idx[i] = i
	}
	if len(keys) < 2 {
		return idx
	}
	family := pyKeyFamily(keys[0])
	for _, k := range keys[1:] {
		if f := pyKeyFamily(k); f != family || f == "none" {
			return idx
		}
	}
	if family == "none" {
		return idx
	}
	for i := 1; i < len(idx); i++ {
		pivot := idx[i]
		lo, hi := 0, i
		for lo < hi {
			mid := (lo + hi) / 2
			if pyKeyLess(keys[pivot], keys[idx[mid]]) {
				hi = mid
			} else {
				lo = mid + 1
			}
		}
		copy(idx[lo+1:i+1], idx[lo:i])
		idx[lo] = pivot
	}
	return idx
}
