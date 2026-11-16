package app

// ptRenderInfo is what the last paint of the input window showed, the WindowRenderInfo vi screen motions read
type ptRenderInfo struct {
	displayedLines []int
	windowHeight   int
	windowWidth    int
	lineHeights    []int
}
