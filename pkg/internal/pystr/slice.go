package pystr

// Code points of s from start to end with Python subscript clamping and negative indexes from the end
func Slice(s string, start, end int) string {
	rs := Runes(s)
	n := len(rs)
	clamp := func(i int) int {
		if i < 0 {
			i += n
		}
		return min(max(i, 0), n)
	}
	a, b := clamp(start), clamp(end)
	if a >= b {
		return ""
	}
	if a == 0 && b == n {
		return s
	}
	return FromRunes(rs[a:b])
}
