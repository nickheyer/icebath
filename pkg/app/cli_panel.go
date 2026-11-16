package app

// panel accumulates the styled fragments of one bordered overlay panel
type panel struct {
	lines  []styledFragment
	border string
	width  int
}
