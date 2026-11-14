package libzstd

// Base frames of a blob file and the damaged frames built from them
type frameSet struct {
	Bases []struct{ Offset, Length int } `json:"bases"`
	blob  []byte
}

// Frame k of the blob with set, flip and cut edits applied in order
func (s *frameSet) frame(k int, ops [][]any) []byte {
	b := append([]byte(nil), s.blob[s.Bases[k].Offset:s.Bases[k].Offset+s.Bases[k].Length]...)
	for _, op := range ops {
		switch op[0].(string) {
		case "flip":
			b[int(op[1].(float64))] ^= 1 << uint(op[2].(float64))
		case "set":
			b[int(op[1].(float64))] = byte(op[2].(float64))
		default:
			b = b[:int(op[1].(float64))]
		}
	}
	return b
}
