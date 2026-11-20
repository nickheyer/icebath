package pyrandom

import (
	"math"
	"math/big"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
)

const (
	mtN         = 624
	mtM         = 397
	mtMatrixA   = 0x9908b0df
	mtUpperMask = 0x80000000
	mtLowerMask = 0x7fffffff
)

// Random is the Mersenne Twister generator behind Python random.Random
type Random struct {
	mt  [mtN]uint32
	mti int
}

// Generator seeded with an integer the way random.seed(n) seeds it
func New(seed int64) *Random {
	r := &Random{}
	r.Seed(seed)
	return r
}

// Reseeds from the absolute value of seed split into little endian 32 bit words
func (r *Random) Seed(seed int64) {
	n := new(big.Int).Abs(big.NewInt(seed))
	var key []uint32
	mask := big.NewInt(0xffffffff)
	for n.Sign() > 0 {
		key = append(key, uint32(new(big.Int).And(n, mask).Uint64()))
		n.Rsh(n, 32)
	}
	if len(key) == 0 {
		key = []uint32{0}
	}
	r.initByArray(key)
}

func (r *Random) initGenrand(s uint32) {
	r.mt[0] = s
	for i := 1; i < mtN; i++ {
		r.mt[i] = 1812433253*(r.mt[i-1]^(r.mt[i-1]>>30)) + uint32(i)
	}
	r.mti = mtN
}

func (r *Random) initByArray(key []uint32) {
	r.initGenrand(19650218)
	i, j := 1, 0
	k := mtN
	if len(key) > k {
		k = len(key)
	}
	for ; k > 0; k-- {
		r.mt[i] = (r.mt[i] ^ ((r.mt[i-1] ^ (r.mt[i-1] >> 30)) * 1664525)) + key[j] + uint32(j)
		i++
		j++
		if i >= mtN {
			r.mt[0] = r.mt[mtN-1]
			i = 1
		}
		if j >= len(key) {
			j = 0
		}
	}
	for k = mtN - 1; k > 0; k-- {
		r.mt[i] = (r.mt[i] ^ ((r.mt[i-1] ^ (r.mt[i-1] >> 30)) * 1566083941)) - uint32(i)
		i++
		if i >= mtN {
			r.mt[0] = r.mt[mtN-1]
			i = 1
		}
	}
	r.mt[0] = 0x80000000
}

// Next tempered 32 bit output of the twister
func (r *Random) genrandUint32() uint32 {
	mag01 := [2]uint32{0, mtMatrixA}
	if r.mti >= mtN {
		var kk int
		for kk = 0; kk < mtN-mtM; kk++ {
			y := (r.mt[kk] & mtUpperMask) | (r.mt[kk+1] & mtLowerMask)
			r.mt[kk] = r.mt[kk+mtM] ^ (y >> 1) ^ mag01[y&1]
		}
		for ; kk < mtN-1; kk++ {
			y := (r.mt[kk] & mtUpperMask) | (r.mt[kk+1] & mtLowerMask)
			r.mt[kk] = r.mt[kk+(mtM-mtN)] ^ (y >> 1) ^ mag01[y&1]
		}
		y := (r.mt[mtN-1] & mtUpperMask) | (r.mt[0] & mtLowerMask)
		r.mt[mtN-1] = r.mt[mtM-1] ^ (y >> 1) ^ mag01[y&1]
		r.mti = 0
	}
	y := r.mt[r.mti]
	r.mti++
	y ^= y >> 11
	y ^= (y << 7) & 0x9d2c5680
	y ^= (y << 15) & 0xefc60000
	y ^= y >> 18
	return y
}

// Non negative integer of k random bits, words filled least significant first as getrandbits does
func (r *Random) GetRandBits(k int) *big.Int {
	if k <= 0 {
		return new(big.Int)
	}
	if k <= 32 {
		return new(big.Int).SetUint64(uint64(r.genrandUint32() >> (32 - k)))
	}
	words := (k-1)/32 + 1
	out := new(big.Int)
	for i := 0; i < words; i++ {
		w := r.genrandUint32()
		if k < 32 {
			w >>= 32 - k
		}
		out.Or(out, new(big.Int).Lsh(new(big.Int).SetUint64(uint64(w)), uint(32*i)))
		k -= 32
	}
	return out
}

// Uniform integer in [0, n) by rejection over n.bit_length() random bits
func (r *Random) RandBelow(n int) int {
	k := big.NewInt(int64(n)).BitLen()
	v := r.GetRandBits(k).Int64()
	for v >= int64(n) {
		v = r.GetRandBits(k).Int64()
	}
	return int(v)
}

// ErrSampleSize is the ValueError random.sample raises for a k outside [0, n]
var ErrSampleSize = &pyint.Error{Kind: "ValueError", Msg: "Sample larger than population or is negative"}

// Indices of k distinct population members chosen as random.sample chooses them, pool or set selection by size
func (r *Random) SampleIndices(n, k int) ([]int, error) {
	if k < 0 || k > n {
		return nil, ErrSampleSize
	}
	result := make([]int, k)
	setsize := 21
	if k > 5 {
		setsize += int(math.Pow(4, math.Ceil(math.Log(float64(k*3))/math.Log(4))))
	}
	if n <= setsize {
		pool := make([]int, n)
		for i := range pool {
			pool[i] = i
		}
		for i := 0; i < k; i++ {
			j := r.RandBelow(n - i)
			result[i] = pool[j]
			pool[j] = pool[n-i-1]
		}
		return result, nil
	}
	selected := map[int]bool{}
	for i := 0; i < k; i++ {
		j := r.RandBelow(n)
		for selected[j] {
			j = r.RandBelow(n)
		}
		selected[j] = true
		result[i] = j
	}
	return result, nil
}

// Sample returns k distinct members of population in random.sample order
func Sample[T any](r *Random, population []T, k int) ([]T, error) {
	idx, err := r.SampleIndices(len(population), k)
	if err != nil {
		return nil, err
	}
	out := make([]T, len(idx))
	for i, j := range idx {
		out[i] = population[j]
	}
	return out, nil
}
