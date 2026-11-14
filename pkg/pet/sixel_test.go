package pet

type sixelLcg struct{ s uint32 }

// Next byte of the generator the recording script uses to build its frames
func (g *sixelLcg) byte() uint8 {
	g.s = (g.s*1103515245 + 12345) & 0x7FFFFFFF
	return uint8(g.s >> 16)
}
