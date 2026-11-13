package edgetts

import (
	"encoding/binary"
	"encoding/hex"
	"math/bits"
	"strconv"
	"strings"
	"time"
)

const (
	trustedClientToken = "6A5AA1D4EAFF4E9FB37E23D68491D6F4"
	windowsEpochOffset = 11644473600
	tokenWindowSeconds = 300
	ticksPerSecond     = 10_000_000
)

var (
	shaInitial = [8]uint32{0x6A09E667, 0xBB67AE85, 0x3C6EF372, 0xA54FF53A, 0x510E527F, 0x9B05688C, 0x1F83D9AB, 0x5BE0CD19}
	shaSeed    = [8]uint32{0xFD9FBEA2, 0x91243CE7, 0xBE499094, 0xCD2E1E0A, 0x6C006F49, 0x04BD35AB, 0xFEB0A7E4, 0x03EFFBD2}
	shaPrefix  = [14]uint32{
		0xEB24BF9E, 0x1558691F, 0xA0C09EE2, 0x0CFA32BA, 0xA622EE8C, 0xD1DD9491, 0xC0DD25A0,
		0xF9E70AB6, 0xF0C31573, 0x7058CE4E, 0x7188E606, 0x00E5AB33, 0xD258B060, 0x72766564,
	}
	shaRound = [64]uint32{
		0x428A2F98, 0x71374491, 0xB5C0FBCF, 0xE9B5DBA5, 0x3956C25B, 0x59F111F1, 0x923F82A4, 0xAB1C5ED5,
		0xD807AA98, 0x12835B01, 0x243185BE, 0x550C7DC3, 0x72BE5D74, 0x80DEB1FE, 0x9BDC06A7, 0xC19BF174,
		0xE49B69C1, 0xEFBE4786, 0x0FC19DC6, 0x240CA1CC, 0x2DE92C6F, 0x4A7484AA, 0x5CB0A9DC, 0x76F988DA,
		0x983E5152, 0xA831C66D, 0xB00327C8, 0xBF597FC7, 0xC6E00BF3, 0xD5A79147, 0x06CA6351, 0x14292967,
		0x27B70A85, 0x2E1B2138, 0x4D2C6DFC, 0x53380D13, 0x650A7354, 0x766A0ABB, 0x81C2C92E, 0x92722C85,
		0xA2BFE8A1, 0xA81A664B, 0xC24B8B70, 0xC76C51A3, 0xD192E819, 0xD6990624, 0xF40E3585, 0x106AA070,
		0x19A4C116, 0x1E376C08, 0x2748774C, 0x34B0BCB5, 0x391C0CB3, 0x4ED8AA4A, 0x5B9CCA4F, 0x682E6FF3,
		0x748F82EE, 0x78A5636F, 0x84C87814, 0x8CC70208, 0x90BEFFFA, 0xA4506CEB, 0xBEF9A3F7, 0xC67178F2,
	}
	checksumCoefficients = [14][2]uint32{
		{0xE82E91B1, 0xB32700D0}, {0x44DE24C6, 0x88DFF51D}, {0x3BEB65BC, 0x29514038}, {0xC89C5933, 0xB328CBDF},
		{0x8D1EC211, 0xD405B4D5}, {0x314EF5D5, 0x26E55F3C}, {0xE92491B1, 0x607801C6}, {0xF0A5B9D5, 0x82B2C1FB},
		{0x06064CF3, 0xFA51A3DA}, {0xDA79FA60, 0x207CC464}, {0xBED20560, 0x3C3C74F3}, {0x89F9C9F8, 0xD2D14B61},
		{0x35FA38C6, 0xBB04A2C1}, {0xCB49E3C6, 0xC5626167},
	}
)

const (
	shaSeedRound        = 14
	shaSeedConstant     = 0xC9DC06A7
	shaSigma0Adjustment = 0x059C0B80
	shaSigma1Adjustment = 0x000B92C0
	checksumFactor      = 0x82893933
	checksumAdjustment0 = 0xF2F90B1A
	checksumAdjustment1 = 0x0B1A0000
)

func smallSigma0(x uint32) uint32 { return bits.RotateLeft32(x, -7) ^ bits.RotateLeft32(x, -18) ^ x>>3 }

func smallSigma1(x uint32) uint32 {
	return bits.RotateLeft32(x, -17) ^ bits.RotateLeft32(x, -19) ^ x>>10
}

// Fills schedule words from index first to 63 with the SHA-256 recurrence
func expandSchedule(w *[64]uint32, first int) {
	for i := first; i < 64; i++ {
		w[i] = smallSigma1(w[i-2]) + w[i-7] + smallSigma0(w[i-15]) + w[i-16]
	}
}

// Runs SHA-256 compression rounds from round first, the seeded entry swapping one round constant
func compress(state [8]uint32, w *[64]uint32, first int) [8]uint32 {
	a, b, c, d, e, f, g, h := state[0], state[1], state[2], state[3], state[4], state[5], state[6], state[7]
	for i := first; i < 64; i++ {
		k := shaRound[i]
		if first == shaSeedRound && i == shaSeedRound {
			k = shaSeedConstant
		}
		t1 := h + (bits.RotateLeft32(e, -6) ^ bits.RotateLeft32(e, -11) ^ bits.RotateLeft32(e, -25)) + (g ^ (e & (f ^ g))) + k + w[i]
		t2 := (bits.RotateLeft32(a, -2) ^ bits.RotateLeft32(a, -13) ^ bits.RotateLeft32(a, -22)) + ((a & b) | (c & (a | b)))
		a, b, c, d, e, f, g, h = t1+t2, a, b, c, d+t1, e, f, g
	}
	return [8]uint32{a, b, c, d, e, f, g, h}
}

// Chaining state after the service's hidden first block, which ends with the first seven salt bytes
func seededState(salt []byte) [8]uint32 {
	var w [64]uint32
	w[14] = uint32(salt[0])<<16 | uint32(salt[1])<<8 | uint32(salt[2])
	w[15] = binary.BigEndian.Uint32(salt[3:7])
	for i := 16; i < 30; i++ {
		value := shaPrefix[i-16]
		if i-2 >= 14 {
			s := smallSigma1(w[i-2])
			if i == 16 {
				s ^= shaSigma1Adjustment
			}
			value += s
		}
		if i-7 >= 14 {
			value += w[i-7]
		}
		if i-15 >= 14 {
			s := smallSigma0(w[i-15])
			if i == 29 {
				s ^= shaSigma0Adjustment
			}
			value += s
		}
		w[i] = value
	}
	w[30] = smallSigma1(w[28]) + w[23] + smallSigma0(w[15]) + w[14] + shaSeedConstant - shaRound[14]
	expandSchedule(&w, 31)
	working := compress(shaSeed, &w, shaSeedRound)
	var out [8]uint32
	for i := range out {
		out[i] = shaInitial[i] + working[i]
	}
	return out
}

// Keyed SHA-256 of the token salt as the read aloud service computes it
func saltDigest(salt []byte) [32]byte {
	length := uint64(57 + len(salt))
	padded := append([]byte(nil), salt[7:]...)
	padded = append(padded, 0x80)
	padded = append(padded, make([]byte, (64+(55-int(length%64)))%64)...)
	padded = binary.BigEndian.AppendUint64(padded, length*8)
	state := seededState(salt[:7])
	for offset := 0; offset < len(padded); offset += 64 {
		var w [64]uint32
		for i := 0; i < 16; i++ {
			w[i] = binary.BigEndian.Uint32(padded[offset+4*i:])
		}
		expandSchedule(&w, 16)
		working := compress(state, &w, 0)
		for i := range state {
			state[i] += working[i]
		}
	}
	var out [32]byte
	for i, word := range state {
		binary.BigEndian.PutUint32(out[4*i:], word)
	}
	return out
}

func swap16(x uint32) uint32 { return bits.RotateLeft32(x, 16) }

// Two word checksum of the salt keyed by the first eight digest bytes
func saltChecksum(salt []byte, digest [32]byte) (uint32, uint32) {
	keyA := binary.LittleEndian.Uint32(digest[0:]) | 1
	keyB := binary.LittleEndian.Uint32(digest[4:]) | 1
	factors := [2]uint32{keyA * checksumFactor, keyB * checksumFactor}
	terms := make([]uint32, 0, len(checksumCoefficients)+len(salt)/4+2)
	offsets := make([]uint32, 0, cap(terms))
	for i, pair := range checksumCoefficients {
		terms = append(terms, pair[0]*factors[i%2])
		offsets = append(offsets, pair[1])
	}
	padded := append([]byte{0}, salt...)
	for len(padded)%8 != 0 {
		padded = append(padded, 0)
	}
	for i := 0; i < len(padded)/4; i++ {
		word := binary.LittleEndian.Uint32(padded[4*i:])
		key, mul := keyA, uint32(0xEF0569FB)
		if i%2 == 1 {
			key, mul = keyB, 0xC31713DB
		}
		term, offset := word*key, swap16(word)*mul
		if i == 0 {
			term += checksumAdjustment0 * factors[0]
			offset += checksumAdjustment1
		}
		terms = append(terms, term)
		offsets = append(offsets, offset)
	}
	var swapAcc, swapLast, state, acc uint32
	for i := 0; i < len(terms); i += 2 {
		x, y := terms[i], terms[i+1]
		q := swap16(offsets[i]+x) * 0x76F50F34
		swapAcc += swap16(q)*0xC3EFEA97 + q
		q = swap16(offsets[i+1]+y) * 0x1881CE3C
		swapLast = swap16(q)*0x35BD1EC9 + q
		swapAcc += swapLast
		state = state*keyA + x
		state = swap16(state) * 0xCF98B111
		state = swap16(state) * 0x87085B9F
		state = swap16(state) * 0x12CEB96D
		state = swap16(state) * 0x257E1D83
		acc += state
		t := state*keyB + y
		t = swap16(t) * 0xA27416F5
		t = swap16(t) * 0xD38396FF
		state = swap16(swap16(t)*0x7C932B89) * 0xBFA49F69
		acc += state
	}
	return swapLast ^ state, swapAcc ^ acc
}

// Sec-MS-GEC value for the five minute window holding the instant
func secMSGEC(at time.Time) string {
	seconds := at.Unix() + windowsEpochOffset
	seconds -= seconds % tokenWindowSeconds
	salt := []byte(strconv.FormatInt(seconds*ticksPerSecond, 10) + trustedClientToken)
	digest := saltDigest(salt)
	a, b := saltChecksum(salt, digest)
	var checksum [8]byte
	binary.LittleEndian.PutUint32(checksum[0:], a)
	binary.LittleEndian.PutUint32(checksum[4:], b)
	return strings.ToUpper(hex.EncodeToString(checksum[:])) + "-" + strings.ToUpper(hex.EncodeToString(digest[:]))
}
