package pypil

import (
	"errors"
	"image"
	"math"
	"sort"
)

type quantPixel struct{ v [4]uint8 }

type quantHashNode struct {
	next  *quantHashNode
	key   quantPixel
	value uint32
}

type quantHash struct {
	table  []*quantHashNode
	length uint32
	count  uint32
	scale  uint32
}

type quantPixelList struct {
	next, prev [3]*quantPixelList
	p          quantPixel
	flag       bool
	count      int
}

type quantBox struct {
	l, r       *quantBox
	head, tail [3]*quantPixelList
	axis       int
	volume     int
	pixelCount uint32
}

// Quantized holds the palette indices of every pixel and the RGB palette Pillow builds for them
type Quantized struct {
	W, H    int
	Pix     []uint8
	Palette []uint8
}

// MemoryError is the Python MemoryError Pillow raises for an image too large to quantize
type MemoryError struct{}

// Python prints a MemoryError raised without arguments as the empty string
func (MemoryError) Error() string { return "" }

const (
	quantMaxHashEntries = 65536
	quantMinLength      = 11
	quantResizeFactor   = 3
)

// The Quant.c PIXEL_HASH of three channel values in 32 bit unsigned arithmetic
func quantPixelHash(r, g, b uint8) uint32 {
	return uint32(r)*463 ^ (uint32(g)<<8)*10069 ^ (uint32(b)<<16)*64997
}

// Hash of a pixel after its channels are shifted right by the table scale
func (h *quantHash) hash(p quantPixel) uint32 {
	return quantPixelHash(p.v[0]>>h.scale, p.v[1]>>h.scale, p.v[2]>>h.scale)
}

// Orders two pixels by their scaled hashes, zero when the hashes are equal
func (h *quantHash) cmp(a, b quantPixel) int {
	x, y := h.hash(a), h.hash(b)
	if x == y {
		return 0
	}
	if x < y {
		return -1
	}
	return 1
}

// The QuantHash.c table size search, which accepts the first candidate with an allowed low nibble
func quantFindPrime(start uint32, dir int) uint32 {
	unit := [16]int{0, 1, 0, 1, 0, 0, 0, 1, 0, 1, 0, 1, 0, 1, 0, 0}
	for start > 1 {
		if unit[start&0x0f] == 0 {
			start += uint32(dir)
			continue
		}
		break
	}
	return start
}

// Links a node into its sorted bucket chain, merging values into an equal key when asked
func (h *quantHash) insertNode(node *quantHashNode, merge bool) {
	n := &h.table[h.hash(node.key)%h.length]
	for ; *n != nil; n = &(*n).next {
		nv := *n
		i := h.cmp(nv.key, node.key)
		if i == 0 {
			nv.key = node.key
			if merge {
				nv.value += node.value
			} else {
				nv.value = node.value
			}
			return
		} else if i > 0 {
			break
		}
	}
	node.next = *n
	*n = node
	h.count++
}

// Rebuilds the table at a new size by reinserting every node in bucket order
func (h *quantHash) rehash(merge bool, newSize uint32) {
	old := h.table
	h.table = make([]*quantHashNode, newSize)
	h.length = newSize
	h.count = 0
	for _, n := range old {
		for n != nil {
			nn := n.next
			n.next = nil
			h.insertNode(n, merge)
			n = nn
		}
	}
}

// Shrinks or grows the table when the entry count leaves the factor three band
func (h *quantHash) resize() {
	oldSize := h.length
	newSize := oldSize
	if h.count*quantResizeFactor < h.length {
		newSize = quantFindPrime(h.length/2-1, -1)
	} else if h.length*quantResizeFactor < h.count {
		newSize = quantFindPrime(h.length*2+1, +1)
	}
	if newSize < quantMinLength {
		newSize = oldSize
	}
	if newSize != oldSize {
		h.rehash(false, newSize)
	}
}

// Adds one to the count stored for a pixel, creating the entry at one
func (h *quantHash) countPixel(key quantPixel) {
	n := &h.table[h.hash(key)%h.length]
	for ; *n != nil; n = &(*n).next {
		i := h.cmp((*n).key, key)
		if i == 0 {
			(*n).value++
			return
		} else if i > 0 {
			break
		}
	}
	*n = &quantHashNode{next: *n, key: key, value: 1}
	h.count++
	h.resize()
}

// Stores a value for a pixel, replacing the value of an existing entry
func (h *quantHash) insert(key quantPixel, val uint32) {
	n := &h.table[h.hash(key)%h.length]
	for ; *n != nil; n = &(*n).next {
		i := h.cmp((*n).key, key)
		if i == 0 {
			(*n).value = val
			return
		} else if i > 0 {
			break
		}
	}
	*n = &quantHashNode{next: *n, key: key, value: val}
	h.count++
	h.resize()
}

// Finds the value stored for a pixel and reports whether an entry exists
func (h *quantHash) lookup(key quantPixel) (uint32, bool) {
	for n := h.table[h.hash(key)%h.length]; n != nil; n = n.next {
		i := h.cmp(n.key, key)
		if i == 0 {
			return n.value, true
		} else if i > 0 {
			break
		}
	}
	return 0, false
}

// Counts every pixel colour, coarsening the channel scale while more than 65536 entries exist
func quantCreatePixelHash(pixels []quantPixel) *quantHash {
	h := &quantHash{table: make([]*quantHashNode, quantMinLength), length: quantMinLength}
	for _, p := range pixels {
		h.countPixel(p)
		for h.count > quantMaxHashEntries {
			h.scale++
			h.rehash(true, h.length)
		}
	}
	return h
}

// Sorts one axis of a pixel list in descending channel order with the Quant.c merge
func quantMergesort(head *quantPixelList, i int) *quantPixelList {
	if head == nil || head.next[i] == nil {
		if head != nil {
			head.next[i] = nil
			head.prev[i] = nil
		}
		return head
	}
	c, t := head, head
	for c != nil && t != nil {
		c = c.next[i]
		if t.next[i] != nil {
			t = t.next[i].next[i]
		} else {
			t = nil
		}
	}
	if c != nil {
		if c.prev[i] != nil {
			c.prev[i].next[i] = nil
		}
		c.prev[i] = nil
	}
	a := quantMergesort(head, i)
	b := quantMergesort(c, i)
	head = nil
	var p *quantPixelList
	for a != nil && b != nil {
		if a.p.v[i] > b.p.v[i] {
			c = a
			a = a.next[i]
		} else {
			c = b
			b = b.next[i]
		}
		c.prev[i] = p
		c.next[i] = nil
		if p != nil {
			p.next[i] = c
		}
		p = c
		if head == nil {
			head = c
		}
	}
	if a != nil {
		c.next[i] = a
		a.prev[i] = c
	} else if b != nil {
		c.next[i] = b
		b.prev[i] = c
	}
	return head
}

// Volume of the colour box spanned by its three sorted lists, cached after the first call
func (b *quantBox) boxVolume() int {
	if b.volume >= 0 {
		return b.volume
	}
	if b.head[0] == nil {
		b.volume = 0
	} else {
		rh, rl := int(b.head[0].p.v[0]), int(b.tail[0].p.v[0])
		gh, gl := int(b.head[1].p.v[1]), int(b.tail[1].p.v[1])
		bh, bl := int(b.head[2].p.v[2]), int(b.tail[2].p.v[2])
		b.volume = (rh - rl + 1) * (gh - gl + 1) * (bh - bl + 1)
	}
	return b.volume
}

// Divides the three lists of a box at the median pixel count along one axis
func quantSplitLists(h, t [3]*quantPixelList, axis int, pixelCount uint32) (nh, nt [2][3]*quantPixelList, nCount [2]uint32) {
	nRight := 0
	var left uint32
	c := h[axis]
	for c != nil {
		left += uint32(c.count)
		nCount[0] += uint32(c.count)
		c.flag = false
		c = c.next[axis]
		if left*2 > pixelCount {
			break
		}
	}
	if c != nil {
		splitColourVal := c.prev[axis].p.v[axis]
		for ; c != nil; c = c.next[axis] {
			if splitColourVal != c.p.v[axis] {
				break
			}
			c.flag = false
			nCount[0] += uint32(c.count)
		}
	}
	for ; c != nil; c = c.next[axis] {
		c.flag = true
		nRight++
		nCount[1] += uint32(c.count)
	}
	if nRight == 0 {
		splitColourVal := t[axis].p.v[axis]
		for c = t[axis]; c != nil; c = c.prev[axis] {
			if splitColourVal != c.p.v[axis] {
				break
			}
			c.flag = true
			nRight++
			nCount[0] -= uint32(c.count)
			nCount[1] += uint32(c.count)
		}
	}
	for i := 0; i < 3; i++ {
		var l, r, n *quantPixelList
		for c = h[i]; c != nil; c = n {
			n = c.next[i]
			if c.flag {
				if r != nil {
					r.next[i] = c
				} else {
					nh[1][i] = c
				}
				c.prev[i] = r
				r = c
			} else {
				if l != nil {
					l.next[i] = c
				} else {
					nh[0][i] = c
				}
				c.prev[i] = l
				l = c
			}
		}
		if l != nil {
			l.next[i] = nil
		}
		if r != nil {
			r.next[i] = nil
		}
		nt[0][i] = l
		nt[1][i] = r
	}
	return nh, nt, nCount
}

// Splits a box along the axis with the widest luminance weighted range into two children
func (node *quantBox) split() {
	rh, rl := int(node.head[0].p.v[0]), int(node.tail[0].p.v[0])
	gh, gl := int(node.head[1].p.v[1]), int(node.tail[1].p.v[1])
	bh, bl := int(node.head[2].p.v[2]), int(node.tail[2].p.v[2])
	f := [3]int{(rh - rl) * 77, (gh - gl) * 150, (bh - bl) * 29}
	best, axis := f[0], 0
	for i := 1; i < 3; i++ {
		if best < f[i] {
			best = f[i]
			axis = i
		}
	}
	node.axis = axis
	heads, tails, counts := quantSplitLists(node.head, node.tail, axis, node.pixelCount)
	left := &quantBox{axis: -1, volume: -1, pixelCount: counts[0]}
	right := &quantBox{axis: -1, volume: -1, pixelCount: counts[1]}
	for i := 0; i < 3; i++ {
		left.head[i], left.tail[i] = heads[0][i], tails[0][i]
		right.head[i], right.tail[i] = heads[1][i], tails[1][i]
		node.head[i], node.tail[i] = nil, nil
	}
	node.l, node.r = left, right
}

type quantHeap struct {
	heap  []*quantBox
	count int
}

// Orders boxes by pixel count the way box_heap_cmp subtracts the two counts as ints
func quantBoxCmp(a, b *quantBox) int { return int(int32(a.pixelCount)) - int(int32(b.pixelCount)) }

// Removes the box with the largest pixel count using the QuantHeap.c sift down
func (h *quantHeap) remove() (*quantBox, bool) {
	if h.count == 0 {
		return nil, false
	}
	r := h.heap[1]
	v := h.heap[h.count]
	h.count--
	k := 1
	for k*2 <= h.count {
		l := k * 2
		if l < h.count {
			if quantBoxCmp(h.heap[l], h.heap[l+1]) < 0 {
				l++
			}
		}
		if quantBoxCmp(v, h.heap[l]) > 0 {
			break
		}
		h.heap[k] = h.heap[l]
		k = l
	}
	h.heap[k] = v
	return r, true
}

// Adds a box using the QuantHeap.c sift up, which stops at a parent of equal count
func (h *quantHeap) add(val *quantBox) {
	h.count++
	for len(h.heap) <= h.count {
		h.heap = append(h.heap, nil)
	}
	k := h.count
	for k != 1 {
		if quantBoxCmp(val, h.heap[k/2]) <= 0 {
			break
		}
		h.heap[k] = h.heap[k/2]
		k >>= 1
	}
	h.heap[k] = val
}

// Builds the box tree by repeatedly splitting the most populated box until the colour budget is used
func quantMedianCut(hl [3]*quantPixelList, imPixelCount uint32, nPixels int) *quantBox {
	root := &quantBox{axis: -1, volume: -1, pixelCount: imPixelCount}
	for i := 0; i < 3; i++ {
		tl := hl[i]
		for tl != nil && tl.next[i] != nil {
			tl = tl.next[i]
		}
		root.head[i] = hl[i]
		root.tail[i] = tl
	}
	h := &quantHeap{heap: make([]*quantBox, 256)}
	h.add(root)
	for nPixels--; nPixels != 0; nPixels-- {
		var thisNode *quantBox
		for {
			var ok bool
			thisNode, ok = h.remove()
			if !ok {
				return root
			}
			if thisNode.boxVolume() != 1 {
				break
			}
		}
		thisNode.split()
		h.add(thisNode.l)
		h.add(thisNode.r)
	}
	return root
}

// Rewrites each hash value to the index of the leaf box holding that colour
func quantAnnotate(n *quantBox, h *quantHash, box *uint32) bool {
	if n.l != nil && n.r != nil {
		return quantAnnotate(n.l, h, box) && quantAnnotate(n.r, h, box)
	}
	if n.l != nil || n.r != nil {
		return false
	}
	for p := n.head[0]; p != nil; p = p.next[0] {
		var q quantPixel
		q.v[0] = p.p.v[0] << h.scale
		q.v[1] = p.p.v[1] << h.scale
		q.v[2] = p.p.v[2] << h.scale
		h.insert(q, *box)
	}
	if n.head[0] != nil {
		*box++
	}
	return true
}

// Squared RGB distance between two pixels as Quant.c _DISTSQR computes it
func quantDistSqr(a, b quantPixel) uint32 {
	dr := int(a.v[0]) - int(b.v[0])
	dg := int(a.v[1]) - int(b.v[1])
	db := int(a.v[2]) - int(b.v[2])
	return uint32(dr*dr + dg*dg + db*db)
}

// Averages the pixels of every box into its palette entry with Pillow rounding
func quantPaletteFromBoxes(pixels []quantPixel, h *quantHash, n uint32) ([]quantPixel, bool) {
	count := make([]uint32, n)
	var avg [3][]uint32
	for i := range avg {
		avg[i] = make([]uint32, n)
	}
	for _, px := range pixels {
		e, ok := h.lookup(px)
		if !ok || e >= n {
			return nil, false
		}
		avg[0][e] += uint32(px.v[0])
		avg[1][e] += uint32(px.v[1])
		avg[2][e] += uint32(px.v[2])
		count[e]++
	}
	p := make([]quantPixel, n)
	for i := uint32(0); i < n; i++ {
		p[i].v[0] = uint8(int(.5 + float64(avg[0][i])/float64(count[i])))
		p[i].v[1] = uint8(int(.5 + float64(avg[1][i])/float64(count[i])))
		p[i].v[2] = uint8(int(.5 + float64(avg[2][i])/float64(count[i])))
	}
	return p, true
}

// Maps every pixel to its nearest palette entry, searching outward from the entry of its box
func quantMapFromBoxes(pixels []quantPixel, pal []quantPixel, h *quantHash) ([]uint32, bool) {
	n := uint32(len(pal))
	avgDist := make([]uint32, n*n)
	for i := uint32(0); i < n; i++ {
		for j := uint32(0); j < i; j++ {
			d := quantDistSqr(pal[i], pal[j])
			avgDist[j*n+i] = d
			avgDist[i*n+j] = d
		}
	}
	order := make([]uint32, n*n)
	for i := uint32(0); i < n; i++ {
		row := order[i*n : (i+1)*n]
		for j := range row {
			row[j] = uint32(j)
		}
		dist := avgDist[i*n : (i+1)*n]
		sort.Slice(row, func(a, b int) bool {
			if dist[row[a]] == dist[row[b]] {
				return row[a] < row[b]
			}
			return dist[row[a]] < dist[row[b]]
		})
	}
	out := make([]uint32, len(pixels))
	seen := map[[3]uint8]uint32{}
	for i, px := range pixels {
		key := [3]uint8{px.v[0], px.v[1], px.v[2]}
		if v, ok := seen[key]; ok {
			out[i] = v
			continue
		}
		pixelVal, ok := h.lookup(px)
		if !ok {
			return nil, false
		}
		initialdist := quantDistSqr(pal[pixelVal], px)
		bestdist := initialdist
		bestmatch := pixelVal
		initialdist <<= 2
		row := order[pixelVal*n : (pixelVal+1)*n]
		dist := avgDist[pixelVal*n : (pixelVal+1)*n]
		for _, idx := range row {
			if dist[idx] > initialdist {
				break
			}
			d := quantDistSqr(pal[idx], px)
			if d < bestdist {
				bestdist = d
				bestmatch = idx
			}
		}
		out[i] = bestmatch
		seen[key] = bestmatch
	}
	return out, true
}

// QuantizeMedianCut ports convert RGB then quantize with MEDIANCUT and no k-means from Pillow 12.3.0
func QuantizeMedianCut(img *image.NRGBA, colors int) (*Quantized, error) {
	if colors < 1 || colors > 256 {
		return nil, errors.New("bad number of colors")
	}
	b := img.Bounds()
	xsize, ysize := b.Dx(), b.Dy()
	if xsize > math.MaxInt32/ysize {
		return nil, MemoryError{}
	}
	pixels := make([]quantPixel, xsize*ysize)
	i := 0
	for y := 0; y < ysize; y++ {
		row := img.Pix[(y)*img.Stride:]
		for x := 0; x < xsize; x++ {
			pixels[i].v = [4]uint8{row[x*4], row[x*4+1], row[x*4+2], 255}
			i++
		}
	}
	h := quantCreatePixelHash(pixels)
	var hl [3]*quantPixelList
	for _, n := range h.table {
		for ; n != nil; n = n.next {
			p := &quantPixelList{count: int(int32(n.value))}
			p.p.v[0] = n.key.v[0] >> h.scale
			p.p.v[1] = n.key.v[1] >> h.scale
			p.p.v[2] = n.key.v[2] >> h.scale
			for a := 0; a < 3; a++ {
				p.next[a] = hl[a]
				if hl[a] != nil {
					hl[a].prev[a] = p
				}
				hl[a] = p
			}
		}
	}
	if hl[0] == nil {
		return nil, errors.New("quantization error")
	}
	for a := 0; a < 3; a++ {
		hl[a] = quantMergesort(hl[a], a)
	}
	root := quantMedianCut(hl, uint32(len(pixels)), colors)
	var nEntries uint32
	if !quantAnnotate(root, h, &nEntries) {
		return nil, errors.New("quantization error")
	}
	pal, ok := quantPaletteFromBoxes(pixels, h, nEntries)
	if !ok {
		return nil, errors.New("quantization error")
	}
	qp, ok := quantMapFromBoxes(pixels, pal, h)
	if !ok {
		return nil, errors.New("quantization error")
	}
	out := &Quantized{W: xsize, H: ysize, Pix: make([]uint8, len(qp)), Palette: make([]uint8, 0, len(pal)*3)}
	for k, v := range qp {
		out.Pix[k] = uint8(v)
	}
	for _, p := range pal {
		out.Palette = append(out.Palette, p.v[0], p.v[1], p.v[2])
	}
	return out, nil
}
