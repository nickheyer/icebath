package pynumpy

import "math"

// Top twelve bits of a double, its sign and biased exponent
func top12(x float64) uint32 {
	return uint32(math.Float64bits(x) >> 52)
}

// Natural exponential as the x86_64 FMA build of glibc 2.44 exp rounds it, the libm call numpy makes
func glibcExp(x float64) float64 {
	abstop := top12(x) & 0x7ff
	if abstop-top12(0x1p-54) >= top12(512.0)-top12(0x1p-54) {
		if abstop-top12(0x1p-54) >= 0x80000000 {
			return 1.0 + x
		}
		if abstop >= top12(1024.0) {
			if math.Float64bits(x) == math.Float64bits(math.Inf(-1)) {
				return 0.0
			}
			if abstop >= top12(math.Inf(1)) {
				return 1.0 + x
			}
			if math.Float64bits(x)>>63 != 0 {
				return 0.0
			}
			return math.Inf(1)
		}
		abstop = 0
	}
	kd := math.FMA(expInvLn2N, x, expShift)
	ki := math.Float64bits(kd)
	kd -= expShift
	r := math.FMA(kd, expNegLn2loN, math.FMA(kd, expNegLn2hiN, x))
	idx := 2 * (ki % expN)
	top := ki << (52 - expTableBits)
	tail := math.Float64frombits(expTab[idx])
	sbits := expTab[idx+1] + top
	r2 := float64(r * r)
	tmp := math.FMA(float64(r2*r2), math.FMA(r, expC5, expC4), math.FMA(r2, math.FMA(r, expC3, expC2), tail+r))
	if abstop == 0 {
		return expSpecialCase(tmp, sbits, ki)
	}
	scale := math.Float64frombits(sbits)
	return math.FMA(scale, tmp, scale)
}

// Scale times one plus tmp for results that overflow or fall subnormal, rounded once before denormalising
func expSpecialCase(tmp float64, sbits, ki uint64) float64 {
	if ki&0x80000000 == 0 {
		sbits -= 1009 << 52
		scale := math.Float64frombits(sbits)
		return 0x1p1009 * math.FMA(scale, tmp, scale)
	}
	sbits += 1022 << 52
	scale := math.Float64frombits(sbits)
	st := float64(scale * tmp)
	y := scale + st
	if y < 1.0 {
		lo := float64(scale-y) + st
		hi := 1.0 + y
		lo = float64(float64(1.0-hi)+y) + lo
		y = float64(hi+lo) - 1.0
		if y == 0.0 {
			y = 0.0
		}
	}
	return 0x1p-1022 * y
}

// Double with its high 32 bits replaced and the low word kept
func setHighWord(x float64, hi uint32) float64 {
	return math.Float64frombits(uint64(hi)<<32 | math.Float64bits(x)&0xffffffff)
}

// Log of one plus x as the x86_64 FMA build of glibc 2.44 log1p rounds it, the libm call numpy makes
func glibcLog1p(x float64) float64 {
	var f, c, u float64
	hx := int32(math.Float64bits(x) >> 32)
	ax := hx & 0x7fffffff
	k := int32(1)
	var hu int32
	if hx < 0x3FDA827A {
		if ax >= 0x3ff00000 {
			if x == -1.0 {
				return math.Inf(-1)
			}
			return (x - x) / (x - x)
		}
		if ax < 0x3e200000 {
			if ax < 0x3c900000 {
				return x
			}
			return math.FMA(-float64(x*x), 0.5, x)
		}
		if hx > 0 || hx <= int32(-0x402d413d) {
			k, f, hu = 0, x, 1
		}
	} else if hx >= 0x7ff00000 {
		return x + x
	}
	if k != 0 {
		if hx < 0x43400000 {
			u = 1.0 + x
			hu = int32(math.Float64bits(u) >> 32)
			k = (hu >> 20) - 1023
			if k > 0 {
				c = 1.0 - (u - x)
			} else {
				c = x - (u - 1.0)
			}
			c /= u
		} else {
			u = x
			hu = int32(math.Float64bits(u) >> 32)
			k = (hu >> 20) - 1023
			c = 0
		}
		hu &= 0x000fffff
		if hu < 0x6a09e {
			u = setHighWord(u, uint32(hu|0x3ff00000))
		} else {
			k++
			u = setHighWord(u, uint32(hu|0x3fe00000))
			hu = (0x00100000 - hu) >> 2
		}
		f = u - 1.0
	}
	hfsq := float64(float64(0.5*f) * f)
	dk := float64(k)
	if hu == 0 {
		if f == 0 {
			if k == 0 {
				return 0
			}
			c = math.FMA(dk, log1pLn2Lo, c)
			return math.FMA(dk, log1pLn2Hi, c)
		}
		r := float64(hfsq * math.FMA(-0.66666666666666666, f, 1.0))
		if k == 0 {
			return f - r
		}
		return math.FMA(dk, log1pLn2Hi, -float64(float64(r-math.FMA(dk, log1pLn2Lo, c))-f))
	}
	s := f / (2.0 + f)
	z := float64(s * s)
	z2 := float64(z * z)
	r2 := math.FMA(z, log1pLp[3], log1pLp[2])
	z4 := float64(z2 * z2)
	r3 := math.FMA(z, log1pLp[5], log1pLp[4])
	z6 := float64(z4 * z2)
	r4 := math.FMA(z, log1pLp[7], log1pLp[6])
	r := math.FMA(z6, r4, math.FMA(z4, r3, math.FMA(z, log1pLp[1], float64(z2*r2))))
	sr := float64(s * (hfsq + r))
	if k == 0 {
		return f - float64(hfsq-sr)
	}
	return math.FMA(dk, log1pLn2Hi, -float64(float64(hfsq-float64(sr+math.FMA(dk, log1pLn2Lo, c)))-f))
}
