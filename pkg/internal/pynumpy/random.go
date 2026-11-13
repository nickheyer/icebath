package pynumpy

import "math/bits"

const (
	seedPoolSize = 4
	seedInitA    = 0x43b0d7e5
	seedMultA    = 0x931e8875
	seedInitB    = 0x8b51f9dd
	seedMultB    = 0x58f38ded
	seedMixMultL = 0xca01f9dd
	seedMixMultR = 0x4973f715
	seedXShift   = 16

	pcgMultHigh = 2549297995355413924
	pcgMultLow  = 4865540595714422341
)

// Entropy pool of numpy.random.SeedSequence with the default pool size and no spawn key
type seedSequence struct {
	pool [seedPoolSize]uint32
}

// Words of a non negative integer seed lowest 32 bits first, a zero seed giving one zero word
func intEntropy(n uint64) []uint32 {
	if n == 0 {
		return []uint32{0}
	}
	var words []uint32
	for n > 0 {
		words = append(words, uint32(n))
		n >>= 32
	}
	return words
}

// Hashes one word with the running multiplier, advancing the multiplier in place
func hashmix(value uint32, hashConst *uint32) uint32 {
	value ^= *hashConst
	*hashConst *= seedMultA
	value *= *hashConst
	value ^= value >> seedXShift
	return value
}

// Combines two pool words with the fixed left and right multipliers
func mix(x, y uint32) uint32 {
	result := seedMixMultL*x - seedMixMultR*y
	result ^= result >> seedXShift
	return result
}

// Pool mixed from the entropy words in SeedSequence.mix_entropy order
func newSeedSequence(entropy []uint32) *seedSequence {
	s := &seedSequence{}
	hashConst := uint32(seedInitA)
	for i := range s.pool {
		if i < len(entropy) {
			s.pool[i] = hashmix(entropy[i], &hashConst)
		} else {
			s.pool[i] = hashmix(0, &hashConst)
		}
	}
	for iSrc := range s.pool {
		for iDst := range s.pool {
			if iSrc != iDst {
				s.pool[iDst] = mix(s.pool[iDst], hashmix(s.pool[iSrc], &hashConst))
			}
		}
	}
	for iSrc := len(s.pool); iSrc < len(entropy); iSrc++ {
		for iDst := range s.pool {
			s.pool[iDst] = mix(s.pool[iDst], hashmix(entropy[iSrc], &hashConst))
		}
	}
	return s
}

// Seeding words as generate_state with dtype uint64 returns them, little endian pairs of 32 bit words
func (s *seedSequence) generateState64(nWords int) []uint64 {
	words := make([]uint32, nWords*2)
	hashConst := uint32(seedInitB)
	for i := range words {
		dataVal := s.pool[i%seedPoolSize]
		dataVal ^= hashConst
		hashConst *= seedMultB
		dataVal *= hashConst
		dataVal ^= dataVal >> seedXShift
		words[i] = dataVal
	}
	out := make([]uint64, nWords)
	for i := range out {
		out[i] = uint64(words[2*i]) | uint64(words[2*i+1])<<32
	}
	return out
}

// PCG64 bit generator with 128 bit LCG state and increment and the XSL RR output
type pcg64 struct {
	stateHigh, stateLow, incHigh, incLow uint64
}

// Generator seeded from four SeedSequence words as state high, state low, increment high, increment low
func newPCG64(seq *seedSequence) *pcg64 {
	val := seq.generateState64(4)
	p := &pcg64{}
	p.incHigh = val[2]<<1 | val[3]>>63
	p.incLow = val[3]<<1 | 1
	p.step()
	var carry uint64
	p.stateLow, carry = bits.Add64(p.stateLow, val[1], 0)
	p.stateHigh, _ = bits.Add64(p.stateHigh, val[0], carry)
	p.step()
	return p
}

// Advances the state by state times the default 128 bit multiplier plus the increment
func (p *pcg64) step() {
	high, low := bits.Mul64(p.stateLow, pcgMultLow)
	high += p.stateHigh*pcgMultLow + p.stateLow*pcgMultHigh
	var carry uint64
	p.stateLow, carry = bits.Add64(low, p.incLow, 0)
	p.stateHigh, _ = bits.Add64(high, p.incHigh, carry)
}

// Next 64 bit output, the high and low state halves xored and rotated right by the top six bits
func (p *pcg64) nextUint64() uint64 {
	p.step()
	return bits.RotateLeft64(p.stateHigh^p.stateLow, -int(p.stateHigh>>58))
}

// Uniform double in zero to one from the top 53 bits of the next output
func (p *pcg64) nextDouble() float64 {
	return float64(p.nextUint64()>>11) * (1.0 / 9007199254740992.0)
}

// Generator draws variates in the order numpy.random.Generator draws them from its PCG64
type Generator struct {
	bitGen *pcg64
}

// Generator numpy.random.default_rng returns for a non negative integer seed
func DefaultRNG(seed uint64) *Generator {
	return &Generator{bitGen: newPCG64(newSeedSequence(intEntropy(seed)))}
}

// Float64 standard normal samples as Generator.standard_normal(size) draws them, negative sizes raising ValueError
func (g *Generator) StandardNormal(size int) ([]float64, error) {
	if size < 0 {
		return nil, &ValueError{Msg: "negative dimensions are not allowed"}
	}
	out := make([]float64, size)
	for i := range out {
		out[i] = g.standardNormal()
	}
	return out, nil
}

// One ziggurat normal draw with numpy's 256 layer tables, exponential tail and wedge rejection
func (g *Generator) standardNormal() float64 {
	for {
		r := g.bitGen.nextUint64()
		idx := r & 0xff
		r >>= 8
		sign := r & 0x1
		rabs := (r >> 1) & 0x000fffffffffffff
		x := float64(float64(rabs) * wiDouble[idx])
		if sign&0x1 != 0 {
			x = -x
		}
		if rabs < kiDouble[idx] {
			return x
		}
		if idx == 0 {
			for {
				xx := float64(-zigguratNorInvR * glibcLog1p(-g.bitGen.nextDouble()))
				yy := -glibcLog1p(-g.bitGen.nextDouble())
				if yy+yy > float64(xx*xx) {
					if (rabs>>8)&0x1 != 0 {
						return -(zigguratNorR + xx)
					}
					return zigguratNorR + xx
				}
			}
		}
		if float64(float64(fiDouble[idx-1]-fiDouble[idx])*g.bitGen.nextDouble())+fiDouble[idx] < glibcExp(float64(-0.5*x)*x) {
			return x
		}
	}
}
