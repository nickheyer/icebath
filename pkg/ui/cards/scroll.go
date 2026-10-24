package cards

// Offset of the first line in view over a column of lines, kept inside the content
type scroll struct {
	offset int
	height int
	total  int
}

func (s *scroll) YOffset() int { return s.offset }
func (s *scroll) Height() int  { return s.height }

// Largest offset that still fills the view
func (s *scroll) maxOffset() int { return max(s.total-s.height, 0) }

func (s *scroll) SetYOffset(n int) { s.offset = min(max(n, 0), s.maxOffset()) }

// Sets the rows in view, keeping the offset inside the content
func (s *scroll) SetHeight(h int) {
	s.height = max(h, 0)
	s.SetYOffset(s.offset)
}

// Sets the lines of content, keeping the offset inside them
func (s *scroll) SetTotal(n int) {
	s.total = max(n, 0)
	s.SetYOffset(s.offset)
}

func (s *scroll) AtBottom() bool   { return s.offset >= s.maxOffset() }
func (s *scroll) GotoBottom()      { s.offset = s.maxOffset() }
func (s *scroll) GotoTop()         { s.offset = 0 }
func (s *scroll) ScrollDown(n int) { s.SetYOffset(s.offset + n) }
func (s *scroll) ScrollUp(n int)   { s.SetYOffset(s.offset - n) }
func (s *scroll) PageDown()        { s.ScrollDown(s.height) }
func (s *scroll) PageUp()          { s.ScrollUp(s.height) }
