package libbz2

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// bz2.compress(b"hello hello hello world\n") from CPython 3.14.8 with libbzip2 1.0.8
const helloStream = "425a6839314159265359a4d7968400000451800010400006449080200031064c40c83116cb675090a07a2f177245385090a4d79684"

// One block with the randomised bit set and both CRCs fixed so CPython decodes it
const randomisedStream = "425a68313141592653597df5ca2c8003f6998040007fe03e2f9c403000b80a00000c99050000064c80a554001a00cd46143ba87550f4a1b5437286f50e0a1c5439287350e8a1d543250d1439a868a1ed43450c287a50c2866a19a87050cd430a1a287450c287e50d6a19287f1772453850907df5ca2c"

// SHA-256 and length of the bytes CPython decodes from the randomised stream
const randomisedSHA, randomisedLen = "8e226efe2f424b5f649a9b97dee1852fd73881bcfd669b82e181695724eb07f4", 2310

// A whole stream in one call reaches StreamEnd with every input byte consumed
func TestDecompressWholeStream(t *testing.T) {
	in := mustHex(t, helloStream)
	out := make([]byte, 100)
	d := NewDecompressor()
	defer d.End()
	nin, nout, ret := d.Decompress(in, out)
	if ret != StreamEnd || nin != len(in) || string(out[:nout]) != "hello hello hello world\n" {
		t.Fatalf("ret %d nin %d out %q", ret, nin, out[:nout])
	}
	if _, _, ret := d.Decompress(nil, out); ret != SequenceError {
		t.Fatalf("after end %d", ret)
	}
}

// Bytes after the end of stream marker stay unconsumed
func TestDecompressStopsAtStreamEnd(t *testing.T) {
	stream := mustHex(t, helloStream)
	in := append(append([]byte{}, stream...), stream...)
	out := make([]byte, 100)
	d := NewDecompressor()
	defer d.End()
	nin, nout, ret := d.Decompress(in, out)
	if ret != StreamEnd || nin != len(stream) || nout != 24 {
		t.Fatalf("ret %d nin %d nout %d", ret, nin, nout)
	}
}

// A randomised block decodes to the bytes CPython returns
func TestDecompressRandomisedBlock(t *testing.T) {
	in := mustHex(t, randomisedStream)
	out := make([]byte, 4096)
	d := NewDecompressor()
	defer d.End()
	nin, nout, ret := d.Decompress(in, out)
	sum := sha256.Sum256(out[:nout])
	if ret != StreamEnd || nin != len(in) || nout != randomisedLen || hex.EncodeToString(sum[:]) != randomisedSHA {
		t.Fatalf("ret %d nin %d nout %d", ret, nin, nout)
	}
}

// One input byte and seven output bytes per call yield the same stream as a single call
func TestDecompressResumesAcrossInputAndOutputSplits(t *testing.T) {
	in := mustHex(t, randomisedStream)
	d := NewDecompressor()
	defer d.End()
	var got []byte
	out := make([]byte, 7)
	pos := 0
	for {
		nin, nout, ret := d.Decompress(in[pos:min(pos+1, len(in))], out)
		pos += nin
		got = append(got, out[:nout]...)
		if ret == StreamEnd {
			break
		}
		if ret != OK || (nin == 0 && nout == 0 && pos == len(in)) {
			t.Fatalf("ret %d at %d", ret, pos)
		}
	}
	sum := sha256.Sum256(got)
	if pos != len(in) || len(got) != randomisedLen || hex.EncodeToString(sum[:]) != randomisedSHA {
		t.Fatalf("pos %d len %d", pos, len(got))
	}
}

// A bad signature or block size digit is a magic error
func TestDecompressBadMagic(t *testing.T) {
	for _, head := range []string{"XZh9", "BZh0", "BZx9"} {
		d := NewDecompressor()
		_, _, ret := d.Decompress([]byte(head+"1AY&SY"), make([]byte, 10))
		d.End()
		if ret != DataErrorMagic {
			t.Fatalf("%s: %d", head, ret)
		}
	}
}

// A damaged block CRC or stream CRC is a data error and the stream is unusable afterwards
func TestDecompressChecksumMismatch(t *testing.T) {
	for _, at := range []int{10, -1} {
		in := mustHex(t, helloStream)
		if at < 0 {
			at = len(in) - 1
		}
		in[at] ^= 0x01
		d := NewDecompressor()
		_, _, ret := d.Decompress(in, make([]byte, 100))
		if ret != DataError {
			t.Fatalf("byte %d: %d", at, ret)
		}
		if _, _, ret := d.Decompress(in, make([]byte, 100)); ret != SequenceError {
			t.Fatalf("byte %d reuse: %d", at, ret)
		}
		d.End()
	}
}

// Ending a decoder suspended mid stream releases it and refuses further input
func TestEndReleasesSuspendedDecoder(t *testing.T) {
	in := mustHex(t, randomisedStream)
	d := NewDecompressor()
	if _, _, ret := d.Decompress(in[:40], make([]byte, 10)); ret != OK {
		t.Fatalf("partial %d", ret)
	}
	d.End()
	if _, _, ret := d.Decompress(in[40:], make([]byte, 10)); ret != SequenceError {
		t.Fatalf("after end %d", ret)
	}
}

// A block whose output fills the buffer exactly still checks its CRC in the same call
func TestDecompressExactOutputChecksBlockCRC(t *testing.T) {
	in := mustHex(t, helloStream)
	in[11] ^= 0x10
	d := NewDecompressor()
	defer d.End()
	out := make([]byte, 24)
	_, nout, ret := d.Decompress(in, out)
	if ret != DataError || !bytes.Equal(out[:nout], []byte("hello hello hello world\n")) {
		t.Fatalf("ret %d out %q", ret, out[:nout])
	}
}
