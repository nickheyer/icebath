package ziputil

import (
	"archive/zip"
	"cmp"
	"slices"
)

// Where member data lies: the member whose data starts at each offset with each length, and the members whose data runs into another member, the way overlapping-entry zip bombs are built
type layout struct {
	at      map[dataSpan]*zip.File
	overlap map[*zip.File]bool
}

// Start and length of a member's stored data in the archive
type dataSpan struct{ start, size int64 }

// Data placement of every member whose local header can be read; a member that carries data overlaps when its data reaches into the 30 bytes before the next member's data, which always belong to that member's local header, or further, or when another member that carries data starts its data at the same offset, duplicate names included
func (r *Reader) placeMembers() *layout {
	type span struct {
		f          *zip.File
		start, end int64
	}
	l := &layout{at: make(map[dataSpan]*zip.File, len(r.File)), overlap: map[*zip.File]bool{}}
	spans := make([]span, 0, len(r.File))
	for _, f := range r.File {
		start, err := f.DataOffset()
		if err != nil {
			continue
		}
		key := dataSpan{start, int64(f.CompressedSize64)}
		if _, seen := l.at[key]; !seen {
			l.at[key] = f
		}
		if f.CompressedSize64 == 0 {
			continue
		}
		spans = append(spans, span{f, start, start + int64(min(f.CompressedSize64, uint64(r.size)))})
	}
	slices.SortStableFunc(spans, func(a, b span) int { return cmp.Compare(a.start, b.start) })
	for i := 0; i < len(spans); {
		j := i + 1
		for j < len(spans) && spans[j].start == spans[i].start {
			j++
		}
		for _, s := range spans[i:j] {
			if j-i > 1 || j < len(spans) && s.end > spans[j].start-localLen {
				l.overlap[s.f] = true
			}
		}
		i = j
	}
	return l
}
