package imaging

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"sort"
)

// Algorithm that picks an adaptive palette
type QuantizeMethod uint8

// Palette construction methods
const (
	// Median cut over colour boxes split along the widest luminance weighted axis, alpha ignored
	MedianCut QuantizeMethod = iota
	// Two level colour cube keeping coarse coverage and the most used fine cells, alpha included
	FastOctree
)

// Reduces the image to an adaptive palette of at most the colour count, nearest entry mapping, no dithering
func Quantize(img image.Image, colors int, m QuantizeMethod) (*image.Paletted, error) {
	if colors < 1 || colors > 256 {
		return nil, fmt.Errorf("%w: palette of %d colours", ErrUnsupported, colors)
	}
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return nil, fmt.Errorf("%w: empty image", ErrUnsupported)
	}
	if m == FastOctree {
		return quantizeOctree(ToNRGBA(img), colors, hasAlpha(img)), nil
	}
	return quantizeMedianCut(ToRGB(img), colors)
}

// Colour key of a pixel in the median cut hash
type qpixel [3]uint8

// Chained hash node counting one colour
type qnode struct {
	next  *qnode
	key   qpixel
	value uint32
}

// Colour hash whose channels coarsen by a shift while it holds too many entries
type qhash struct {
	table  []*qnode
	length uint32
	count  uint32
	scale  uint32
}

// Entry ceiling and sizing constants of the colour hash
const (
	qMaxEntries   = 65536
	qMinLength    = 11
	qResizeFactor = 3
)

// Hash of three channel values in 32 bit unsigned arithmetic
func qmix(r, g, b uint8) uint32 {
	return uint32(r)*463 ^ (uint32(g)<<8)*10069 ^ (uint32(b)<<16)*64997
}

// Hash of a colour after its channels shift right by the table scale
func (h *qhash) hash(p qpixel) uint32 {
	return qmix(p[0]>>h.scale, p[1]>>h.scale, p[2]>>h.scale)
}

// Orders two colours by their scaled hashes, zero when the hashes are equal
func (h *qhash) cmp(a, b qpixel) int {
	x, y := h.hash(a), h.hash(b)
	switch {
	case x == y:
		return 0
	case x < y:
		return -1
	}
	return 1
}

// Table size search accepting the first candidate whose low nibble is allowed
func qprime(start uint32, dir int) uint32 {
	unit := [16]bool{1: true, 3: true, 7: true, 9: true, 11: true, 13: true}
	for start > 1 && !unit[start&0x0f] {
		start = uint32(int(start) + dir)
	}
	return start
}

// Links a node into its sorted chain, merging or replacing the value of an equal key
func (h *qhash) link(node *qnode, merge bool) {
	n := &h.table[h.hash(node.key)%h.length]
	for ; *n != nil; n = &(*n).next {
		c := h.cmp((*n).key, node.key)
		if c == 0 {
			(*n).key = node.key
			if merge {
				(*n).value += node.value
			} else {
				(*n).value = node.value
			}
			return
		}
		if c > 0 {
			break
		}
	}
	node.next = *n
	*n = node
	h.count++
}

// Rebuilds the table at a size by relinking every node in bucket order
func (h *qhash) rehash(merge bool, size uint32) {
	old := h.table
	h.table = make([]*qnode, size)
	h.length, h.count = size, 0
	for _, n := range old {
		for n != nil {
			next := n.next
			n.next = nil
			h.link(n, merge)
			n = next
		}
	}
}

// Shrinks or grows the table once the entry count leaves the factor three band
func (h *qhash) resize() {
	size := h.length
	if h.count*qResizeFactor < h.length {
		size = qprime(h.length/2-1, -1)
	} else if h.length*qResizeFactor < h.count {
		size = qprime(h.length*2+1, 1)
	}
	if size < qMinLength {
		size = h.length
	}
	if size != h.length {
		h.rehash(false, size)
	}
}

// Stores a value for a colour, counting up from one when increment is set
func (h *qhash) put(key qpixel, value uint32, increment bool) {
	n := &h.table[h.hash(key)%h.length]
	for ; *n != nil; n = &(*n).next {
		c := h.cmp((*n).key, key)
		if c == 0 {
			if increment {
				(*n).value++
			} else {
				(*n).value = value
			}
			return
		}
		if c > 0 {
			break
		}
	}
	*n = &qnode{next: *n, key: key, value: value}
	h.count++
	h.resize()
}

// Value stored for a colour
func (h *qhash) lookup(key qpixel) (uint32, bool) {
	for n := h.table[h.hash(key)%h.length]; n != nil; n = n.next {
		c := h.cmp(n.key, key)
		if c == 0 {
			return n.value, true
		}
		if c > 0 {
			break
		}
	}
	return 0, false
}

// Element of the three axis sorted colour lists
type qlist struct {
	next, prev [3]*qlist
	p          qpixel
	flag       bool
	count      int
}

// Colour box of the median cut tree
type qbox struct {
	l, r       *qbox
	head, tail [3]*qlist
	volume     int
	pixels     uint32
}

// Sorts one axis of a list in descending channel order with a stable merge
func qsortList(head *qlist, i int) *qlist {
	if head == nil || head.next[i] == nil {
		if head != nil {
			head.next[i], head.prev[i] = nil, nil
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
	a, b := qsortList(head, i), qsortList(c, i)
	head = nil
	var p *qlist
	for a != nil && b != nil {
		if a.p[i] > b.p[i] {
			c, a = a, a.next[i]
		} else {
			c, b = b, b.next[i]
		}
		c.prev[i], c.next[i] = p, nil
		if p != nil {
			p.next[i] = c
		}
		p = c
		if head == nil {
			head = c
		}
	}
	if a != nil {
		c.next[i], a.prev[i] = a, c
	} else if b != nil {
		c.next[i], b.prev[i] = b, c
	}
	return head
}

// Volume of the box spanned by its sorted lists, cached after the first call
func (b *qbox) boxVolume() int {
	if b.volume >= 0 {
		return b.volume
	}
	b.volume = 0
	if b.head[0] != nil {
		b.volume = 1
		for i := 0; i < 3; i++ {
			b.volume *= int(b.head[i].p[i]) - int(b.tail[i].p[i]) + 1
		}
	}
	return b.volume
}

// Divides the three lists of a box at the median pixel count along one axis
func qsplitLists(h, t [3]*qlist, axis int, pixels uint32) (nh, nt [2][3]*qlist, counts [2]uint32) {
	right := 0
	var left uint32
	c := h[axis]
	for c != nil {
		left += uint32(c.count)
		counts[0] += uint32(c.count)
		c.flag = false
		c = c.next[axis]
		if left*2 > pixels {
			break
		}
	}
	if c != nil {
		split := c.prev[axis].p[axis]
		for ; c != nil && c.p[axis] == split; c = c.next[axis] {
			c.flag = false
			counts[0] += uint32(c.count)
		}
	}
	for ; c != nil; c = c.next[axis] {
		c.flag = true
		right++
		counts[1] += uint32(c.count)
	}
	if right == 0 {
		split := t[axis].p[axis]
		for c = t[axis]; c != nil && c.p[axis] == split; c = c.prev[axis] {
			c.flag = true
			counts[0] -= uint32(c.count)
			counts[1] += uint32(c.count)
		}
	}
	for i := 0; i < 3; i++ {
		var l, r, n *qlist
		for c = h[i]; c != nil; c = n {
			n = c.next[i]
			if c.flag {
				if r != nil {
					r.next[i] = c
				} else {
					nh[1][i] = c
				}
				c.prev[i], r = r, c
			} else {
				if l != nil {
					l.next[i] = c
				} else {
					nh[0][i] = c
				}
				c.prev[i], l = l, c
			}
		}
		if l != nil {
			l.next[i] = nil
		}
		if r != nil {
			r.next[i] = nil
		}
		nt[0][i], nt[1][i] = l, r
	}
	return nh, nt, counts
}

// Splits a box along the axis of widest luminance weighted range into two children
func (b *qbox) split() {
	f := [3]int{
		(int(b.head[0].p[0]) - int(b.tail[0].p[0])) * 77,
		(int(b.head[1].p[1]) - int(b.tail[1].p[1])) * 150,
		(int(b.head[2].p[2]) - int(b.tail[2].p[2])) * 29,
	}
	axis := 0
	for i := 1; i < 3; i++ {
		if f[axis] < f[i] {
			axis = i
		}
	}
	heads, tails, counts := qsplitLists(b.head, b.tail, axis, b.pixels)
	left := &qbox{volume: -1, pixels: counts[0]}
	right := &qbox{volume: -1, pixels: counts[1]}
	for i := 0; i < 3; i++ {
		left.head[i], left.tail[i] = heads[0][i], tails[0][i]
		right.head[i], right.tail[i] = heads[1][i], tails[1][i]
		b.head[i], b.tail[i] = nil, nil
	}
	b.l, b.r = left, right
}

// Max heap of boxes by pixel count whose sift stops at equal counts
type qheap struct {
	heap  []*qbox
	count int
}

// Pixel count difference of two boxes as signed ints
func qboxCmp(a, b *qbox) int { return int(int32(a.pixels)) - int(int32(b.pixels)) }

// Removes the box with the largest pixel count
func (h *qheap) pop() (*qbox, bool) {
	if h.count == 0 {
		return nil, false
	}
	top, last := h.heap[1], h.heap[h.count]
	h.count--
	k := 1
	for k*2 <= h.count {
		l := k * 2
		if l < h.count && qboxCmp(h.heap[l], h.heap[l+1]) < 0 {
			l++
		}
		if qboxCmp(last, h.heap[l]) > 0 {
			break
		}
		h.heap[k] = h.heap[l]
		k = l
	}
	h.heap[k] = last
	return top, true
}

// Adds a box, stopping the sift at a parent of equal count
func (h *qheap) push(b *qbox) {
	h.count++
	for len(h.heap) <= h.count {
		h.heap = append(h.heap, nil)
	}
	k := h.count
	for k != 1 && qboxCmp(b, h.heap[k/2]) > 0 {
		h.heap[k] = h.heap[k/2]
		k >>= 1
	}
	h.heap[k] = b
}

// Box tree split from the most populated box until the colour budget is spent
func qmedianCut(lists [3]*qlist, total uint32, colors int) *qbox {
	root := &qbox{volume: -1, pixels: total}
	for i := 0; i < 3; i++ {
		t := lists[i]
		for t != nil && t.next[i] != nil {
			t = t.next[i]
		}
		root.head[i], root.tail[i] = lists[i], t
	}
	h := &qheap{heap: make([]*qbox, 256)}
	h.push(root)
	for n := colors - 1; n != 0; n-- {
		var b *qbox
		for {
			var ok bool
			if b, ok = h.pop(); !ok {
				return root
			}
			if b.boxVolume() != 1 {
				break
			}
		}
		b.split()
		h.push(b.l)
		h.push(b.r)
	}
	return root
}

// Rewrites each hash value to the index of the leaf box holding that colour
func qannotate(b *qbox, h *qhash, next *uint32) bool {
	if b.l != nil && b.r != nil {
		return qannotate(b.l, h, next) && qannotate(b.r, h, next)
	}
	if b.l != nil || b.r != nil {
		return false
	}
	for p := b.head[0]; p != nil; p = p.next[0] {
		h.put(qpixel{p.p[0] << h.scale, p.p[1] << h.scale, p.p[2] << h.scale}, *next, false)
	}
	if b.head[0] != nil {
		*next++
	}
	return true
}

// Squared RGB distance
func qdist(a, b qpixel) uint32 {
	dr, dg, db := int(a[0])-int(b[0]), int(a[1])-int(b[1]), int(a[2])-int(b[2])
	return uint32(dr*dr + dg*dg + db*db)
}

// Error for a palette the median cut could not build
var errQuantize = errors.New("imaging: quantization error")

// Median cut palette of an opaque image and the nearest entry of every pixel, searching outward from its box
func quantizeMedianCut(rgb *image.RGBA, colors int) (*image.Paletted, error) {
	w, hgt := rgb.Rect.Dx(), rgb.Rect.Dy()
	pixels := make([]qpixel, w*hgt)
	for i := range pixels {
		pixels[i] = qpixel{rgb.Pix[4*i], rgb.Pix[4*i+1], rgb.Pix[4*i+2]}
	}
	h := &qhash{table: make([]*qnode, qMinLength), length: qMinLength}
	for _, p := range pixels {
		h.put(p, 1, true)
		for h.count > qMaxEntries {
			h.scale++
			h.rehash(true, h.length)
		}
	}
	var lists [3]*qlist
	for _, n := range h.table {
		for ; n != nil; n = n.next {
			e := &qlist{count: int(int32(n.value)), p: qpixel{n.key[0] >> h.scale, n.key[1] >> h.scale, n.key[2] >> h.scale}}
			for a := 0; a < 3; a++ {
				e.next[a] = lists[a]
				if lists[a] != nil {
					lists[a].prev[a] = e
				}
				lists[a] = e
			}
		}
	}
	if lists[0] == nil {
		return nil, errQuantize
	}
	for a := 0; a < 3; a++ {
		lists[a] = qsortList(lists[a], a)
	}
	root := qmedianCut(lists, uint32(len(pixels)), colors)
	var entries uint32
	if !qannotate(root, h, &entries) {
		return nil, errQuantize
	}
	count := make([]uint32, entries)
	var sums [3][]uint32
	for i := range sums {
		sums[i] = make([]uint32, entries)
	}
	for _, p := range pixels {
		e, ok := h.lookup(p)
		if !ok || e >= entries {
			return nil, errQuantize
		}
		for c := 0; c < 3; c++ {
			sums[c][e] += uint32(p[c])
		}
		count[e]++
	}
	pal := make([]qpixel, entries)
	for i := range pal {
		for c := 0; c < 3; c++ {
			pal[i][c] = uint8(int(0.5 + float64(sums[c][i])/float64(count[i])))
		}
	}
	n := entries
	dist := make([]uint32, n*n)
	for i := uint32(0); i < n; i++ {
		for j := uint32(0); j < i; j++ {
			d := qdist(pal[i], pal[j])
			dist[j*n+i], dist[i*n+j] = d, d
		}
	}
	order := make([]uint32, n*n)
	for i := uint32(0); i < n; i++ {
		row := order[i*n : (i+1)*n]
		for j := range row {
			row[j] = uint32(j)
		}
		d := dist[i*n : (i+1)*n]
		sort.Slice(row, func(a, b int) bool {
			if d[row[a]] == d[row[b]] {
				return row[a] < row[b]
			}
			return d[row[a]] < d[row[b]]
		})
	}
	out := image.NewPaletted(image.Rect(0, 0, w, hgt), nil)
	seen := map[qpixel]uint8{}
	for i, p := range pixels {
		if v, ok := seen[p]; ok {
			out.Pix[i] = v
			continue
		}
		box, ok := h.lookup(p)
		if !ok {
			return nil, errQuantize
		}
		best, bestd := box, qdist(pal[box], p)
		limit := bestd << 2
		d := dist[box*n : (box+1)*n]
		for _, idx := range order[box*n : (box+1)*n] {
			if d[idx] > limit {
				break
			}
			if dd := qdist(pal[idx], p); dd < bestd {
				best, bestd = idx, dd
			}
		}
		out.Pix[i] = uint8(best)
		seen[p] = uint8(best)
	}
	out.Palette = make(color.Palette, n)
	for i, p := range pal {
		out.Palette[i] = color.RGBA{p[0], p[1], p[2], 255}
	}
	return out, nil
}

// Running sums of the pixels falling into one cube cell
type qbucket struct {
	count      uint64
	r, g, b, a uint64
}

// Colour cube of cells addressed by the top bits of each channel
type qcube struct {
	bits    [4]uint
	offset  [4]uint
	buckets []qbucket
}

// Cube with the given bits per channel, red outermost
func newQCube(r, g, b, a uint) *qcube {
	c := &qcube{bits: [4]uint{r, g, b, a}}
	c.offset = [4]uint{g + b + a, b + a, a, 0}
	c.buckets = make([]qbucket, 1<<(r+g+b+a))
	return c
}

// Cell index of channel positions
func (c *qcube) pos(r, g, b, a uint) int {
	return int(r<<c.offset[0] | g<<c.offset[1] | b<<c.offset[2] | a<<c.offset[3])
}

// Cell a colour falls into
func (c *qcube) at(p [4]uint8) *qbucket {
	return &c.buckets[c.pos(uint(p[0])>>(8-c.bits[0]), uint(p[1])>>(8-c.bits[1]), uint(p[2])>>(8-c.bits[2]), uint(p[3])>>(8-c.bits[3]))]
}

// Number of cells holding pixels
func (c *qcube) used() int {
	n := 0
	for _, b := range c.buckets {
		if b.count > 0 {
			n++
		}
	}
	return n
}

// Copy at other bits per channel, coarser cells summing finer ones and finer cells repeating coarser ones
func (c *qcube) resized(r, g, b, a uint) *qcube {
	out := newQCube(r, g, b, a)
	var srcReduce, dstReduce, width [4]uint
	for i, nb := range out.bits {
		if c.bits[i] > nb {
			dstReduce[i], width[i] = c.bits[i]-nb, 1<<c.bits[i]
		} else {
			srcReduce[i], width[i] = nb-c.bits[i], 1<<nb
		}
	}
	for r := uint(0); r < width[0]; r++ {
		for g := uint(0); g < width[1]; g++ {
			for b := uint(0); b < width[2]; b++ {
				for a := uint(0); a < width[3]; a++ {
					s := &c.buckets[c.pos(r>>srcReduce[0], g>>srcReduce[1], b>>srcReduce[2], a>>srcReduce[3])]
					d := &out.buckets[out.pos(r>>dstReduce[0], g>>dstReduce[1], b>>dstReduce[2], a>>dstReduce[3])]
					d.count += s.count
					d.r += s.r
					d.g += s.g
					d.b += s.b
					d.a += s.a
				}
			}
		}
	}
	return out
}

// Average colour of a cell through single precision division, clear black when empty
func (b *qbucket) average() [4]uint8 {
	if b.count == 0 {
		return [4]uint8{}
	}
	n := float32(b.count)
	clip := func(v uint64) uint8 { return uint8(max(0, min(255, int(float32(v)/n)))) }
	return [4]uint8{clip(b.r), clip(b.g), clip(b.b), clip(b.a)}
}

// Cells of a cube by descending pixel count, equal counts kept in cube order
func (c *qcube) sorted() []qbucket {
	out := append([]qbucket(nil), c.buckets...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].count > out[j].count })
	return out
}

// Removes the pixels of the first n cells from the cube cells holding their averages
func (c *qcube) subtract(buckets []qbucket, n int) {
	for i := 0; i < n && i < len(buckets); i++ {
		s := &buckets[i]
		if s.count == 0 {
			continue
		}
		d := c.at(s.average())
		d.count -= s.count
		d.r -= s.r
		d.g -= s.g
		d.b -= s.b
		d.a -= s.a
	}
}

// Writes palette indices into the cells holding each entry average, later entries first
func (c *qcube) addLookups(pal []qbucket, n, offset int) {
	for i := offset + n - 1; i >= offset; i-- {
		c.at(pal[i].average()).count = uint64(i)
	}
}

// Palette of every coarse cell plus the most used fine cells and the pixels mapped through a lookup cube
func quantizeOctree(n *image.NRGBA, colors int, alpha bool) *image.Paletted {
	levels := [8]uint{4, 4, 4, 0, 2, 2, 2, 0}
	if alpha {
		levels = [8]uint{3, 4, 3, 3, 2, 2, 2, 2}
	}
	px := make([][4]uint8, len(n.Pix)/4)
	for i := range px {
		px[i] = [4]uint8{n.Pix[4*i], n.Pix[4*i+1], n.Pix[4*i+2], n.Pix[4*i+3]}
		if !alpha {
			px[i][3] = 255
		}
	}
	fine := newQCube(levels[0], levels[1], levels[2], levels[3])
	for _, p := range px {
		b := fine.at(p)
		b.count++
		b.r += uint64(p[0])
		b.g += uint64(p[1])
		b.b += uint64(p[2])
		b.a += uint64(p[3])
	}
	coarse := fine.resized(levels[4], levels[5], levels[6], levels[7])
	nCoarse := min(coarse.used(), colors)
	nFine := colors - nCoarse
	fineSorted := fine.sorted()
	coarse.subtract(fineSorted, nFine)
	for nCoarse > coarse.used() {
		done := nFine
		nCoarse = coarse.used()
		nFine = colors - nCoarse
		coarse.subtract(fineSorted[min(done, len(fineSorted)):], nFine-done)
	}
	coarseSorted := coarse.sorted()
	pal := append(append([]qbucket(nil), coarseSorted[:nCoarse]...), fineSorted[:min(nFine, len(fineSorted))]...)
	for len(pal) < colors {
		pal = append(pal, qbucket{})
	}
	coarseLookup := newQCube(levels[4], levels[5], levels[6], levels[7])
	coarseLookup.addLookups(pal, nCoarse, 0)
	lookup := coarseLookup.resized(levels[0], levels[1], levels[2], levels[3])
	lookup.addLookups(pal, nFine, nCoarse)
	out := image.NewPaletted(n.Rect, make(color.Palette, colors))
	for i, p := range px {
		out.Pix[i] = uint8(lookup.at(p).count)
	}
	for i := range out.Palette {
		c := pal[i].average()
		if alpha {
			out.Palette[i] = color.NRGBA{c[0], c[1], c[2], c[3]}
		} else {
			out.Palette[i] = color.RGBA{c[0], c[1], c[2], 255}
		}
	}
	return out
}

// Maps the image onto a fixed palette by nearest colour, Floyd Steinberg error diffusion when dithering
func QuantizePalette(img image.Image, palette color.Palette, dither bool) *image.Paletted {
	rgb := ToRGB(img)
	w, h := rgb.Rect.Dx(), rgb.Rect.Dy()
	pal := make([][3]int, min(len(palette), 256))
	for i := range pal {
		c := color.NRGBAModel.Convert(palette[i]).(color.NRGBA)
		pal[i] = [3]int{int(c.R), int(c.G), int(c.B)}
	}
	out := image.NewPaletted(image.Rect(0, 0, w, h), palette[:len(pal)])
	if len(pal) == 0 {
		return out
	}
	cache := newPaletteCache(pal)
	if !dither {
		for i := range out.Pix {
			out.Pix[i] = uint8(cache.nearest(int(rgb.Pix[4*i]), int(rgb.Pix[4*i+1]), int(rgb.Pix[4*i+2])))
		}
		return out
	}
	errs := make([]int, (w+1)*3)
	for y := 0; y < h; y++ {
		var r, r0, r1, g, g0, g1, b, b0, b1 int
		e := 0
		for x := 0; x < w; x++ {
			in := rgb.Pix[y*rgb.Stride+4*x:]
			r = clip8i(int(in[0]) + (r+errs[e+3])/16)
			g = clip8i(int(in[1]) + (g+errs[e+4])/16)
			b = clip8i(int(in[2]) + (b+errs[e+5])/16)
			idx := cache.nearest(r, g, b)
			out.Pix[y*out.Stride+x] = uint8(idx)
			r -= pal[idx][0]
			g -= pal[idx][1]
			b -= pal[idx][2]
			r2 := r
			errs[e], r0, r1, r = 3*r+r0, 5*r+r1, r2, 7*r
			g2 := g
			errs[e+1], g0, g1, g = 3*g+g0, 5*g+g1, g2, 7*g
			b2 := b
			errs[e+2], b0, b1, b = 3*b+b0, 5*b+b1, b2, 7*b
			e += 3
		}
		errs[e], errs[e+1], errs[e+2] = r0, g0, b0
	}
	return out
}

// Integer clamped to one byte
func clip8i(v int) int { return max(0, min(255, v)) }

// Nearest palette entry per four level colour cell, filled a 32 value box at a time
type paletteCache struct {
	pal   [][3]int
	cells []int16
}

// Empty cache over a palette
func newPaletteCache(pal [][3]int) *paletteCache {
	c := &paletteCache{pal: pal, cells: make([]int16, 64*64*64)}
	for i := range c.cells {
		c.cells[i] = -1
	}
	return c
}

// Palette index closest to the colour cell of a colour, the first entry winning ties
func (c *paletteCache) nearest(r, g, b int) int {
	i := r>>2 + (g>>2)*64 + (b>>2)*64*64
	if c.cells[i] < 0 {
		c.fill(r, g, b)
	}
	return int(c.cells[i])
}

// Resolves every cell of the 32 value box around a colour against the lower corner of each cell
func (c *paletteCache) fill(r, g, b int) {
	r0, g0, b0 := r&0xe0, g&0xe0, b&0xe0
	for cr := r0; cr < r0+32; cr += 4 {
		for cg := g0; cg < g0+32; cg += 4 {
			for cb := b0; cb < b0+32; cb += 4 {
				best, bestd := 0, -1
				for i, p := range c.pal {
					dr, dg, db := cr-p[0], cg-p[1], cb-p[2]
					if d := dr*dr + dg*dg + db*db; bestd < 0 || d < bestd {
						best, bestd = i, d
					}
				}
				c.cells[cr>>2+(cg>>2)*64+(cb>>2)*64*64] = int16(best)
			}
		}
	}
}
