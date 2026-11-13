package chrome

// Active match position of a webview that reports only match counts, kept the way WebKit keeps its match index
type FindIndex struct {
	query   string
	ordinal int
}

// One based ordinal of the active match after a successful find of query that counted count matches
func (f *FindIndex) Found(query string, backwards bool, count int) int {
	if query != f.query {
		f.query, f.ordinal = query, 0
	}
	switch {
	case count <= 0:
		f.ordinal = 0
	case f.ordinal == 0:
		f.ordinal = 1
	case backwards && f.ordinal == 1:
		f.ordinal = count
	case backwards:
		f.ordinal--
	default:
		f.ordinal = f.ordinal%count + 1
	}
	return f.ordinal
}

// Forgets the query and the active match after a failed or stopped find, as WebKit hides its find UI
func (f *FindIndex) Reset() { f.query, f.ordinal = "", 0 }
