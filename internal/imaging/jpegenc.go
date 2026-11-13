package imaging

import (
	"bufio"
	"fmt"
	"image"
	"io"
	"math"
)

// Settings of one JPEG encoding
type JPEGOptions struct {
	// Quality from 1 to 100, zero meaning 75
	Quality int
	// Writes successive approximation scans in the standard simple progression, Huffman tables always optimal
	Progressive bool
	// Builds optimal Huffman tables from a counting pass instead of the standard tables
	Optimize bool
}

// Base luminance quantization table in natural order
var lumaQuant = [64]int{
	16, 11, 10, 16, 24, 40, 51, 61,
	12, 12, 14, 19, 26, 58, 60, 55,
	14, 13, 16, 24, 40, 57, 69, 56,
	14, 17, 22, 29, 51, 87, 80, 62,
	18, 22, 37, 56, 68, 109, 103, 77,
	24, 35, 55, 64, 81, 104, 113, 92,
	49, 64, 78, 87, 103, 121, 120, 101,
	72, 92, 95, 98, 112, 100, 103, 99,
}

// Base chrominance quantization table in natural order
var chromaQuant = [64]int{
	17, 18, 24, 47, 99, 99, 99, 99,
	18, 21, 26, 66, 99, 99, 99, 99,
	24, 26, 56, 99, 99, 99, 99, 99,
	47, 66, 99, 99, 99, 99, 99, 99,
	99, 99, 99, 99, 99, 99, 99, 99,
	99, 99, 99, 99, 99, 99, 99, 99,
	99, 99, 99, 99, 99, 99, 99, 99,
	99, 99, 99, 99, 99, 99, 99, 99,
}

// Natural index of each position in diagonal scan order
var zigzag = [64]int{
	0, 1, 8, 16, 9, 2, 3, 10, 17, 24, 32, 25, 18, 11, 4, 5,
	12, 19, 26, 33, 40, 48, 41, 34, 27, 20, 13, 6, 7, 14, 21, 28,
	35, 42, 49, 56, 57, 50, 43, 36, 29, 22, 15, 23, 30, 37, 44, 51,
	58, 59, 52, 45, 38, 31, 39, 46, 53, 60, 61, 54, 47, 55, 62, 63,
}

// Code length counts and symbols of one Huffman table
type huffSpec struct {
	bits [17]int
	vals []byte
}

// Standard Huffman tables, luminance and chrominance DC then AC
var stdHuff = [4]huffSpec{
	{bits: [17]int{0, 0, 1, 5, 1, 1, 1, 1, 1, 1, 0, 0, 0, 0, 0, 0, 0}, vals: []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}},
	{bits: [17]int{0, 0, 3, 1, 1, 1, 1, 1, 1, 1, 1, 1, 0, 0, 0, 0, 0}, vals: []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}},
	{bits: [17]int{0, 0, 2, 1, 3, 3, 2, 4, 3, 5, 5, 4, 4, 0, 0, 1, 0x7d}, vals: []byte{
		0x01, 0x02, 0x03, 0x00, 0x04, 0x11, 0x05, 0x12, 0x21, 0x31, 0x41, 0x06, 0x13, 0x51, 0x61, 0x07,
		0x22, 0x71, 0x14, 0x32, 0x81, 0x91, 0xa1, 0x08, 0x23, 0x42, 0xb1, 0xc1, 0x15, 0x52, 0xd1, 0xf0,
		0x24, 0x33, 0x62, 0x72, 0x82, 0x09, 0x0a, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x25, 0x26, 0x27, 0x28,
		0x29, 0x2a, 0x34, 0x35, 0x36, 0x37, 0x38, 0x39, 0x3a, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48, 0x49,
		0x4a, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58, 0x59, 0x5a, 0x63, 0x64, 0x65, 0x66, 0x67, 0x68, 0x69,
		0x6a, 0x73, 0x74, 0x75, 0x76, 0x77, 0x78, 0x79, 0x7a, 0x83, 0x84, 0x85, 0x86, 0x87, 0x88, 0x89,
		0x8a, 0x92, 0x93, 0x94, 0x95, 0x96, 0x97, 0x98, 0x99, 0x9a, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa7,
		0xa8, 0xa9, 0xaa, 0xb2, 0xb3, 0xb4, 0xb5, 0xb6, 0xb7, 0xb8, 0xb9, 0xba, 0xc2, 0xc3, 0xc4, 0xc5,
		0xc6, 0xc7, 0xc8, 0xc9, 0xca, 0xd2, 0xd3, 0xd4, 0xd5, 0xd6, 0xd7, 0xd8, 0xd9, 0xda, 0xe1, 0xe2,
		0xe3, 0xe4, 0xe5, 0xe6, 0xe7, 0xe8, 0xe9, 0xea, 0xf1, 0xf2, 0xf3, 0xf4, 0xf5, 0xf6, 0xf7, 0xf8,
		0xf9, 0xfa,
	}},
	{bits: [17]int{0, 0, 2, 1, 2, 4, 4, 3, 4, 7, 5, 4, 4, 0, 1, 2, 0x77}, vals: []byte{
		0x00, 0x01, 0x02, 0x03, 0x11, 0x04, 0x05, 0x21, 0x31, 0x06, 0x12, 0x41, 0x51, 0x07, 0x61, 0x71,
		0x13, 0x22, 0x32, 0x81, 0x08, 0x14, 0x42, 0x91, 0xa1, 0xb1, 0xc1, 0x09, 0x23, 0x33, 0x52, 0xf0,
		0x15, 0x62, 0x72, 0xd1, 0x0a, 0x16, 0x24, 0x34, 0xe1, 0x25, 0xf1, 0x17, 0x18, 0x19, 0x1a, 0x26,
		0x27, 0x28, 0x29, 0x2a, 0x35, 0x36, 0x37, 0x38, 0x39, 0x3a, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48,
		0x49, 0x4a, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58, 0x59, 0x5a, 0x63, 0x64, 0x65, 0x66, 0x67, 0x68,
		0x69, 0x6a, 0x73, 0x74, 0x75, 0x76, 0x77, 0x78, 0x79, 0x7a, 0x82, 0x83, 0x84, 0x85, 0x86, 0x87,
		0x88, 0x89, 0x8a, 0x92, 0x93, 0x94, 0x95, 0x96, 0x97, 0x98, 0x99, 0x9a, 0xa2, 0xa3, 0xa4, 0xa5,
		0xa6, 0xa7, 0xa8, 0xa9, 0xaa, 0xb2, 0xb3, 0xb4, 0xb5, 0xb6, 0xb7, 0xb8, 0xb9, 0xba, 0xc2, 0xc3,
		0xc4, 0xc5, 0xc6, 0xc7, 0xc8, 0xc9, 0xca, 0xd2, 0xd3, 0xd4, 0xd5, 0xd6, 0xd7, 0xd8, 0xd9, 0xda,
		0xe2, 0xe3, 0xe4, 0xe5, 0xe6, 0xe7, 0xe8, 0xe9, 0xea, 0xf2, 0xf3, 0xf4, 0xf5, 0xf6, 0xf7, 0xf8,
		0xf9, 0xfa,
	}},
}

// Codes and lengths per symbol derived from a table specification
type huffCodes struct {
	code [256]uint32
	size [256]uint8
}

// Canonical codes of a table specification
func (s *huffSpec) derive() *huffCodes {
	c := &huffCodes{}
	code, p := uint32(0), 0
	for l := 1; l <= 16; l++ {
		for i := 0; i < s.bits[l]; i++ {
			c.code[s.vals[p]], c.size[s.vals[p]] = code, uint8(l)
			code++
			p++
		}
		code <<= 1
	}
	return c
}

// Optimal length limited table for symbol frequencies, the all ones code kept free by a pseudo symbol
func optimalHuff(freq [257]int64) huffSpec {
	const maxLen = 257
	var bits [maxLen + 1]int
	var codesize [257]int
	others := [257]int{}
	for i := range others {
		others[i] = -1
	}
	freq[256] = 1
	used := false
	for _, f := range freq[:256] {
		used = used || f != 0
	}
	if !used {
		freq[0] = 1
	}
	for {
		c1, v := -1, int64(math.MaxInt64)
		for i := 0; i <= 256; i++ {
			if freq[i] != 0 && freq[i] <= v {
				v, c1 = freq[i], i
			}
		}
		c2 := -1
		v = math.MaxInt64
		for i := 0; i <= 256; i++ {
			if freq[i] != 0 && freq[i] <= v && i != c1 {
				v, c2 = freq[i], i
			}
		}
		if c2 < 0 {
			break
		}
		freq[c1] += freq[c2]
		freq[c2] = 0
		codesize[c1]++
		for others[c1] >= 0 {
			c1 = others[c1]
			codesize[c1]++
		}
		others[c1] = c2
		codesize[c2]++
		for others[c2] >= 0 {
			c2 = others[c2]
			codesize[c2]++
		}
	}
	for i := 0; i <= 256; i++ {
		if codesize[i] > 0 {
			bits[codesize[i]]++
		}
	}
	i := maxLen
	for ; i > 16; i-- {
		for bits[i] > 0 {
			j := i - 2
			for bits[j] == 0 {
				j--
			}
			bits[i] -= 2
			bits[i-1]++
			bits[j+1] += 2
			bits[j]--
		}
	}
	for bits[i] == 0 {
		i--
	}
	bits[i]--
	var s huffSpec
	copy(s.bits[:], bits[:17])
	for l := 1; l <= maxLen; l++ {
		for j := 0; j <= 255; j++ {
			if codesize[j] == l {
				s.vals = append(s.vals, byte(j))
			}
		}
	}
	return s
}

// One image component with its sampling factors, tables and quantized blocks over the MCU grid
type jpegComp struct {
	id      byte
	h, v    int
	tq      int
	td, ta  int
	bw, bh  int
	gw, gh  int
	coef    [][64]int32
	samples []int32
	sw      int
}

// Encodes baseline or progressive JFIF, grey as one component and colour as YCbCr with halved chroma
func EncodeJPEG(w io.Writer, img image.Image, o JPEGOptions) error {
	q := o.Quality
	if q == 0 {
		q = 75
	}
	q = max(1, min(100, q))
	scale := 200 - 2*q
	if q < 50 {
		scale = 5000 / q
	}
	var quant [2][64]int
	for i := 0; i < 64; i++ {
		quant[0][i] = max(1, min(255, (lumaQuant[i]*scale+50)/100))
		quant[1][i] = max(1, min(255, (chromaQuant[i]*scale+50)/100))
	}
	b := img.Bounds()
	width, height := b.Dx(), b.Dy()
	if width <= 0 || height <= 0 || width > 65535 || height > 65535 {
		return fmt.Errorf("%w: jpeg of %dx%d pixels", ErrUnsupported, width, height)
	}
	var comps []*jpegComp
	if g, ok := img.(*image.Gray); ok || isGray16(img) {
		if !ok {
			g = toGray(img)
		}
		comps = []*jpegComp{{id: 1, h: 1, v: 1}}
		comps[0].fullSamples(g.Pix, g.Stride, g.Rect.Min, width, height)
	} else {
		rgb := ToRGB(img)
		comps = []*jpegComp{{id: 1, h: 2, v: 2}, {id: 2, h: 1, v: 1, tq: 1, td: 1, ta: 1}, {id: 3, h: 1, v: 1, tq: 1, td: 1, ta: 1}}
		ycc(rgb, comps)
	}
	maxH, maxV := comps[0].h, comps[0].v
	mcuX, mcuY := (width+8*maxH-1)/(8*maxH), (height+8*maxV-1)/(8*maxV)
	for _, c := range comps {
		c.gw, c.gh = mcuX*c.h, mcuY*c.v
		c.transform(quant[c.tq])
	}
	e := &jpegWriter{w: bufio.NewWriter(w)}
	e.marker(0xd8, nil)
	e.marker(0xe0, []byte{'J', 'F', 'I', 'F', 0, 1, 1, 0, 0, 1, 0, 1, 0, 0})
	for t := 0; t <= comps[len(comps)-1].tq; t++ {
		body := []byte{byte(t)}
		for _, z := range zigzag {
			body = append(body, byte(quant[t][z]))
		}
		e.marker(0xdb, body)
	}
	sof := byte(0xc0)
	if o.Progressive {
		sof = 0xc2
	}
	frame := []byte{8, byte(height >> 8), byte(height), byte(width >> 8), byte(width), byte(len(comps))}
	for _, c := range comps {
		frame = append(frame, c.id, byte(c.h<<4|c.v), byte(c.tq))
	}
	e.marker(sof, frame)
	if o.Progressive {
		for _, s := range progression(len(comps)) {
			sc := &jpegScan{comps: pick(comps, s.comps), ss: s.ss, se: s.se, ah: s.ah, al: s.al, progressive: true, mcuX: mcuX, mcuY: mcuY}
			sc.write(e, true)
		}
	} else {
		sc := &jpegScan{comps: comps, ss: 0, se: 63, mcuX: mcuX, mcuY: mcuY}
		sc.write(e, o.Optimize)
	}
	e.marker(0xd9, nil)
	if e.err != nil {
		return e.err
	}
	return e.w.Flush()
}

// Reports a sixteen bit grey image
func isGray16(img image.Image) bool {
	_, ok := img.(*image.Gray16)
	return ok
}

// Full resolution samples of one channel padded to whole blocks by repeating the last row and column
func (c *jpegComp) fullSamples(pix []uint8, stride int, at image.Point, width, height int) {
	c.bw, c.bh = (width+7)/8, (height+7)/8
	c.sw = c.bw * 8
	c.samples = make([]int32, c.sw*c.bh*8)
	for y := 0; y < c.bh*8; y++ {
		sy := min(y, height-1)
		row := pix[(at.Y+sy)*stride+at.X:]
		for x := 0; x < c.sw; x++ {
			c.samples[y*c.sw+x] = int32(row[min(x, width-1)])
		}
	}
}

// Colour converts to YCbCr with fixed point weights, luma at full size and chroma averaged over two by two cells
func ycc(rgb *image.RGBA, comps []*jpegComp) {
	b := rgb.Bounds()
	width, height := b.Dx(), b.Dy()
	ys := make([]uint8, width*height)
	cbs := make([]uint8, width*height)
	crs := make([]uint8, width*height)
	const half = 1 << 15
	fix := func(f float64) int32 { return int32(f*65536 + 0.5) }
	ry, gy, by := fix(0.29900), fix(0.58700), fix(0.11400)
	rcb, gcb := -fix(0.16874), -fix(0.33126)
	bcb := fix(0.5)
	gcr, bcr := -fix(0.41869), -fix(0.08131)
	for y := 0; y < height; y++ {
		row := rgb.Pix[y*rgb.Stride:]
		for x := 0; x < width; x++ {
			r, g, bl := int32(row[4*x]), int32(row[4*x+1]), int32(row[4*x+2])
			i := y*width + x
			ys[i] = uint8((ry*r + gy*g + by*bl + half) >> 16)
			cbs[i] = uint8((rcb*r + gcb*g + bcb*bl + 128<<16 + half - 1) >> 16)
			crs[i] = uint8((bcb*r + gcr*g + bcr*bl + 128<<16 + half - 1) >> 16)
		}
	}
	comps[0].fullSamples(ys, width, image.Point{}, width, height)
	for k, plane := range [][]uint8{cbs, crs} {
		c := comps[k+1]
		cw, ch := (width+1)/2, (height+1)/2
		c.bw, c.bh = (cw+7)/8, (ch+7)/8
		c.sw = c.bw * 8
		c.samples = make([]int32, c.sw*c.bh*8)
		for oy := 0; oy < c.bh*8; oy++ {
			ry := min(oy, ch-1)
			r0, r1 := min(2*ry, height-1), min(2*ry+1, height-1)
			bias := int32(1)
			for ox := 0; ox < c.sw; ox++ {
				c0, c1 := min(2*ox, width-1), min(2*ox+1, width-1)
				s := int32(plane[r0*width+c0]) + int32(plane[r0*width+c1]) + int32(plane[r1*width+c0]) + int32(plane[r1*width+c1])
				c.samples[oy*c.sw+ox] = (s + bias) >> 2
				bias ^= 3
			}
		}
	}
}

// Forward DCT and quantization of every real block, then the DC only dummy blocks the MCU grid needs
func (c *jpegComp) transform(q [64]int) {
	c.coef = make([][64]int32, c.gw*c.gh)
	var block [64]int32
	for by := 0; by < c.bh; by++ {
		for bx := 0; bx < c.bw; bx++ {
			for y := 0; y < 8; y++ {
				row := c.samples[(by*8+y)*c.sw+bx*8:]
				for x := 0; x < 8; x++ {
					block[y*8+x] = row[x] - 128
				}
			}
			fdct(&block)
			out := &c.coef[by*c.gw+bx]
			for i := 0; i < 64; i++ {
				qv := int32(q[i] << 3)
				t := block[i]
				if t < 0 {
					out[i] = -((-t + qv>>1) / qv)
				} else {
					out[i] = (t + qv>>1) / qv
				}
			}
		}
		for bx := c.bw; bx < c.gw; bx++ {
			c.coef[by*c.gw+bx][0] = c.coef[by*c.gw+c.bw-1][0]
		}
	}
	for by := c.bh; by < c.gh; by++ {
		for mx := 0; mx < c.gw/c.h; mx++ {
			dc := c.coef[(by-1)*c.gw+mx*c.h+c.h-1][0]
			for bi := 0; bi < c.h; bi++ {
				c.coef[by*c.gw+mx*c.h+bi][0] = dc
			}
		}
	}
	c.samples = nil
}

// Slow but accurate integer forward DCT, output scaled up by eight
func fdct(d *[64]int32) {
	const (
		constBits = 13
		pass1Bits = 2
		fix0298   = 2446
		fix0390   = 3196
		fix0541   = 4433
		fix0765   = 6270
		fix0899   = 7373
		fix1175   = 9633
		fix1501   = 12299
		fix1847   = 15137
		fix1961   = 16069
		fix2053   = 16819
		fix2562   = 20995
		fix3072   = 25172
	)
	descale := func(x int32, n uint) int32 { return (x + 1<<(n-1)) >> n }
	for r := 0; r < 8; r++ {
		p := d[r*8 : r*8+8]
		t0, t7 := p[0]+p[7], p[0]-p[7]
		t1, t6 := p[1]+p[6], p[1]-p[6]
		t2, t5 := p[2]+p[5], p[2]-p[5]
		t3, t4 := p[3]+p[4], p[3]-p[4]
		t10, t13 := t0+t3, t0-t3
		t11, t12 := t1+t2, t1-t2
		p[0] = (t10 + t11) << pass1Bits
		p[4] = (t10 - t11) << pass1Bits
		z1 := (t12 + t13) * fix0541
		p[2] = descale(z1+t13*fix0765, constBits-pass1Bits)
		p[6] = descale(z1-t12*fix1847, constBits-pass1Bits)
		z1, z2, z3, z4 := t4+t7, t5+t6, t4+t6, t5+t7
		z5 := (z3 + z4) * fix1175
		t4, t5, t6, t7 = t4*fix0298, t5*fix2053, t6*fix3072, t7*fix1501
		z1, z2, z3, z4 = -z1*fix0899, -z2*fix2562, -z3*fix1961, -z4*fix0390
		z3 += z5
		z4 += z5
		p[7] = descale(t4+z1+z3, constBits-pass1Bits)
		p[5] = descale(t5+z2+z4, constBits-pass1Bits)
		p[3] = descale(t6+z2+z3, constBits-pass1Bits)
		p[1] = descale(t7+z1+z4, constBits-pass1Bits)
	}
	for c := 0; c < 8; c++ {
		at := func(i int) *int32 { return &d[i*8+c] }
		t0, t7 := *at(0)+*at(7), *at(0)-*at(7)
		t1, t6 := *at(1)+*at(6), *at(1)-*at(6)
		t2, t5 := *at(2)+*at(5), *at(2)-*at(5)
		t3, t4 := *at(3)+*at(4), *at(3)-*at(4)
		t10, t13 := t0+t3, t0-t3
		t11, t12 := t1+t2, t1-t2
		*at(0) = descale(t10+t11, pass1Bits)
		*at(4) = descale(t10-t11, pass1Bits)
		z1 := (t12 + t13) * fix0541
		*at(2) = descale(z1+t13*fix0765, constBits+pass1Bits)
		*at(6) = descale(z1-t12*fix1847, constBits+pass1Bits)
		z1, z2, z3, z4 := t4+t7, t5+t6, t4+t6, t5+t7
		z5 := (z3 + z4) * fix1175
		t4, t5, t6, t7 = t4*fix0298, t5*fix2053, t6*fix3072, t7*fix1501
		z1, z2, z3, z4 = -z1*fix0899, -z2*fix2562, -z3*fix1961, -z4*fix0390
		z3 += z5
		z4 += z5
		*at(7) = descale(t4+z1+z3, constBits+pass1Bits)
		*at(5) = descale(t5+z2+z4, constBits+pass1Bits)
		*at(3) = descale(t6+z2+z3, constBits+pass1Bits)
		*at(1) = descale(t7+z1+z4, constBits+pass1Bits)
	}
}

// Components, spectral band and successive approximation bits of one progressive scan
type scanSpec struct {
	comps          []int
	ss, se, ah, al int
}

// Scan script of the simple successive approximation layout, the colour form for three components
func progression(n int) []scanSpec {
	all := make([]int, n)
	for i := range all {
		all[i] = i
	}
	if n == 3 {
		return []scanSpec{
			{all, 0, 0, 0, 1}, {[]int{0}, 1, 5, 0, 2}, {[]int{2}, 1, 63, 0, 1}, {[]int{1}, 1, 63, 0, 1},
			{[]int{0}, 6, 63, 0, 2}, {[]int{0}, 1, 63, 2, 1}, {all, 0, 0, 1, 0},
			{[]int{2}, 1, 63, 1, 0}, {[]int{1}, 1, 63, 1, 0}, {[]int{0}, 1, 63, 1, 0},
		}
	}
	var out []scanSpec
	add := func(ss, se, ah, al int) {
		for _, c := range all {
			out = append(out, scanSpec{[]int{c}, ss, se, ah, al})
		}
	}
	out = append(out, scanSpec{all, 0, 0, 0, 1})
	add(1, 5, 0, 2)
	add(6, 63, 0, 2)
	add(1, 63, 2, 1)
	out = append(out, scanSpec{all, 0, 0, 1, 0})
	add(1, 63, 1, 0)
	return out
}

// Components at the given indices
func pick(comps []*jpegComp, idx []int) []*jpegComp {
	out := make([]*jpegComp, len(idx))
	for i, k := range idx {
		out[i] = comps[k]
	}
	return out
}

// Marker and entropy writer with byte stuffing
type jpegWriter struct {
	w     *bufio.Writer
	err   error
	acc   uint32
	nbits uint
}

// Writes a segment with its length prefixed body
func (e *jpegWriter) marker(m byte, body []byte) {
	if e.err != nil {
		return
	}
	if body == nil && (m == 0xd8 || m == 0xd9) {
		_, e.err = e.w.Write([]byte{0xff, m})
		return
	}
	n := len(body) + 2
	if _, e.err = e.w.Write([]byte{0xff, m, byte(n >> 8), byte(n)}); e.err == nil {
		_, e.err = e.w.Write(body)
	}
}

// Appends the low n binary digits of a value, stuffing a zero after every 0xFF byte
func (e *jpegWriter) bits(v uint32, n uint) {
	if n == 0 {
		return
	}
	e.acc = e.acc<<n | v&(1<<n-1)
	e.nbits += n
	for e.nbits >= 8 {
		b := byte(e.acc >> (e.nbits - 8))
		e.nbits -= 8
		if e.err == nil {
			e.err = e.w.WriteByte(b)
		}
		if b == 0xff && e.err == nil {
			e.err = e.w.WriteByte(0)
		}
	}
	e.acc &= 1<<e.nbits - 1
}

// Pads the last byte with one bits
func (e *jpegWriter) flush() {
	if e.nbits > 0 {
		e.bits(0x7f, 8-e.nbits)
	}
}

// Writes a Huffman table definition
func (e *jpegWriter) dht(class, index int, s huffSpec) {
	body := []byte{byte(class<<4 | index)}
	for l := 1; l <= 16; l++ {
		body = append(body, byte(s.bits[l]))
	}
	e.marker(0xc4, append(body, s.vals...))
}

// One scan over a set of components and a band of coefficients
type jpegScan struct {
	comps          []*jpegComp
	ss, se, ah, al int
	progressive    bool
	mcuX, mcuY     int
	gather         bool
	freq           [4][257]int64
	codes          [4]*huffCodes
	out            *jpegWriter
	eobrun         int
	be             []byte
	dcPred         []int32
}

// Huffman table indexes the scan codes with in definition order, DC at 0 and 1, AC at 2 and 3
func (s *jpegScan) slots() []int {
	var order []int
	seen := [4]bool{}
	add := func(slot int) {
		if !seen[slot] {
			seen[slot] = true
			order = append(order, slot)
		}
	}
	for _, c := range s.comps {
		switch {
		case !s.progressive:
			add(c.td)
			add(2 + c.ta)
		case s.ss == 0 && s.ah == 0:
			add(c.td)
		case s.ss > 0:
			add(2 + c.ta)
		}
	}
	return order
}

// Emits or counts one Huffman code
func (s *jpegScan) symbol(slot int, sym int) {
	if s.gather {
		s.freq[slot][sym]++
		return
	}
	s.out.bits(s.codes[slot].code[sym], uint(s.codes[slot].size[sym]))
}

// Emits unencoded bits outside the counting pass
func (s *jpegScan) raw(v uint32, n uint) {
	if !s.gather {
		s.out.bits(v, n)
	}
}

// Writes the tables the scan needs, its header and its entropy coded data
func (s *jpegScan) write(e *jpegWriter, optimal bool) {
	slots := s.slots()
	if optimal {
		s.gather = true
		s.run()
		s.gather = false
	}
	for _, slot := range slots {
		spec := stdHuff[slot]
		if optimal {
			spec = optimalHuff(s.freq[slot])
		}
		e.dht(slot/2, slot%2, spec)
		s.codes[slot] = spec.derive()
	}
	head := []byte{byte(len(s.comps))}
	for _, c := range s.comps {
		td, ta := c.td, c.ta
		if s.progressive {
			if s.ss == 0 {
				ta = 0
				if s.ah != 0 {
					td = 0
				}
			} else {
				td = 0
			}
		}
		head = append(head, c.id, byte(td<<4|ta))
	}
	head = append(head, byte(s.ss), byte(s.se), byte(s.ah<<4|s.al))
	e.marker(0xda, head)
	s.out = e
	s.run()
	e.flush()
}

// Walks the blocks of the scan in coding order, encoding or counting each
func (s *jpegScan) run() {
	s.eobrun, s.be = 0, s.be[:0]
	s.dcPred = make([]int32, len(s.comps))
	if len(s.comps) == 1 {
		c := s.comps[0]
		for by := 0; by < c.bh; by++ {
			for bx := 0; bx < c.bw; bx++ {
				s.block(0, &c.coef[by*c.gw+bx])
			}
		}
	} else {
		for my := 0; my < s.mcuY; my++ {
			for mx := 0; mx < s.mcuX; mx++ {
				for ci, c := range s.comps {
					for v := 0; v < c.v; v++ {
						for h := 0; h < c.h; h++ {
							s.block(ci, &c.coef[(my*c.v+v)*c.gw+mx*c.h+h])
						}
					}
				}
			}
		}
	}
	s.flushEOB()
}

// Encodes one 8 by 8 unit in the mode the scan parameters select
func (s *jpegScan) block(ci int, b *[64]int32) {
	c := s.comps[ci]
	switch {
	case !s.progressive:
		s.dc(ci, c, b[0])
		s.acFirst(c, b)
	case s.ss == 0 && s.ah == 0:
		s.dc(ci, c, b[0]>>s.al)
	case s.ss == 0:
		s.raw(uint32(b[0]>>s.al)&1, 1)
	case s.ah == 0:
		s.acFirst(c, b)
	default:
		s.acRefine(c, b)
	}
}

// Encodes the difference of a DC value from the previous one of the component
func (s *jpegScan) dc(ci int, c *jpegComp, v int32) {
	diff := v - s.dcPred[ci]
	s.dcPred[ci] = v
	t, t2 := diff, diff
	if t < 0 {
		t = -t
		t2--
	}
	n := bitLen(uint32(t))
	s.symbol(c.td, n)
	s.raw(uint32(t2), uint(n))
}

// Number of bits in a magnitude
func bitLen(v uint32) int {
	n := 0
	for v != 0 {
		n++
		v >>= 1
	}
	return n
}

// Encodes the AC band point transformed, sequential scans ending each block and progressive ones holding end of band runs
func (s *jpegScan) acFirst(c *jpegComp, b *[64]int32) {
	slot := 2 + c.ta
	ss, se, al := s.ss, s.se, s.al
	if !s.progressive {
		ss, se, al = 1, 63, 0
	}
	r := 0
	for k := ss; k <= se; k++ {
		t := b[zigzag[k]]
		if t == 0 {
			r++
			continue
		}
		var t2 int32
		if t < 0 {
			t = (-t) >> al
			t2 = ^t
		} else {
			t >>= al
			t2 = t
		}
		if t == 0 {
			r++
			continue
		}
		s.flushEOB()
		for r > 15 {
			s.symbol(slot, 0xf0)
			r -= 16
		}
		n := bitLen(uint32(t))
		s.symbol(slot, r<<4|n)
		s.raw(uint32(t2), uint(n))
		r = 0
	}
	if r == 0 {
		return
	}
	if !s.progressive {
		s.symbol(slot, 0)
		return
	}
	s.eobrun++
	if s.eobrun == 0x7fff {
		s.flushEOB()
	}
}

// Emits a pending end of band run and the correction bits it carries
func (s *jpegScan) flushEOB() {
	if s.eobrun == 0 {
		return
	}
	n := bitLen(uint32(s.eobrun)) - 1
	s.symbol(2+s.comps[0].ta, n<<4)
	if n > 0 {
		s.raw(uint32(s.eobrun), uint(n))
	}
	s.eobrun = 0
	s.emitCorrections(s.be)
	s.be = s.be[:0]
}

// Emits buffered correction bits outside the counting pass
func (s *jpegScan) emitCorrections(bits []byte) {
	for _, b := range bits {
		s.raw(uint32(b), 1)
	}
}

// Encodes one refinement pass over a band, newly significant coefficients coded and older ones refined by one bit
func (s *jpegScan) acRefine(c *jpegComp, b *[64]int32) {
	slot := 2 + c.ta
	var abs [64]int32
	eob := 0
	for k := s.ss; k <= s.se; k++ {
		t := b[zigzag[k]]
		if t < 0 {
			t = -t
		}
		t >>= s.al
		abs[k] = t
		if t == 1 {
			eob = k
		}
	}
	r := 0
	var br []byte
	for k := s.ss; k <= s.se; k++ {
		t := abs[k]
		if t == 0 {
			r++
			continue
		}
		for r > 15 && k <= eob {
			s.flushEOB()
			s.symbol(slot, 0xf0)
			r -= 16
			s.emitCorrections(br)
			br = br[:0]
		}
		if t > 1 {
			br = append(br, byte(t&1))
			continue
		}
		s.flushEOB()
		s.symbol(slot, r<<4|1)
		sign := uint32(1)
		if b[zigzag[k]] < 0 {
			sign = 0
		}
		s.raw(sign, 1)
		s.emitCorrections(br)
		br = br[:0]
		r = 0
	}
	if r > 0 || len(br) > 0 {
		s.eobrun++
		s.be = append(s.be, br...)
		if s.eobrun == 0x7fff || len(s.be) > 1000-64+1 {
			s.flushEOB()
		}
	}
}
