package pyzip

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Decoded bytes and final state of a decompressor fed 997 byte pieces with max_length 1001
type trickleOutcome struct {
	SHA   string `json:"sha"`
	Size  int    `json:"size"`
	Class string `json:"class"`
	Msg   string `json:"msg"`
	EOF   bool   `json:"eof"`
}

// One damaged frame of fuzz.json, a base frame and the edits applied to it
type fuzzCase struct {
	Base    int            `json:"base"`
	Damage  [][]any        `json:"damage"`
	Oneshot pyOutcome      `json:"oneshot"`
	Trickle trickleOutcome `json:"trickle"`
	Zip     streamExpect   `json:"zip"`
}

// Applies flip, set and cut edits in order
func applyDamage(frame []byte, ops [][]any) []byte {
	b := append([]byte(nil), frame...)
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

// Archive of record.py zip_bytes, stored members around one member of the given method
func zipBytes(members [][5]any) []byte {
	le := binary.LittleEndian
	var body, central []byte
	for _, m := range members {
		name, method, raw, crc, size := m[0].(string), m[1].(int), m[2].([]byte), m[3].(uint32), m[4].(int)
		version := map[int]uint16{0: 20, 93: 63}[method]
		offset := len(body)
		h := le.AppendUint32(nil, 0x04034b50)
		h = append(h, byte(version), 0)
		for _, v := range []uint16{0, uint16(method), 0x6000, 0x5A21} {
			h = le.AppendUint16(h, v)
		}
		h = le.AppendUint32(le.AppendUint32(le.AppendUint32(h, crc), uint32(len(raw))), uint32(size))
		h = le.AppendUint16(le.AppendUint16(h, uint16(len(name))), 0)
		body = append(append(append(body, h...), name...), raw...)
		c := le.AppendUint32(nil, 0x02014b50)
		c = append(c, 63, 3, byte(version), 0)
		for _, v := range []uint16{0, uint16(method), 0x6000, 0x5A21} {
			c = le.AppendUint16(c, v)
		}
		c = le.AppendUint32(le.AppendUint32(le.AppendUint32(c, crc), uint32(len(raw))), uint32(size))
		for _, v := range []uint16{uint16(len(name)), 0, 0, 0, 0} {
			c = le.AppendUint16(c, v)
		}
		c = le.AppendUint32(le.AppendUint32(c, 0o100644<<16), uint32(offset))
		central = append(append(central, c...), name...)
	}
	end := le.AppendUint32(nil, 0x06054b50)
	for _, v := range []uint16{0, 0, uint16(len(members)), uint16(len(members))} {
		end = le.AppendUint16(end, v)
	}
	end = le.AppendUint16(le.AppendUint32(le.AppendUint32(end, uint32(len(central))), uint32(len(body))), 0)
	return append(append(body, central...), end...)
}

// Feeds frame to a fresh decompressor in 997 byte pieces with max_length 1001 until it ends, fails or starves
func trickleDecode(frame []byte) trickleOutcome {
	d := newZstdDecompressor()
	var got []byte
	res := trickleOutcome{}
	pos := 0
	for i := 0; i < 100000 && !d.isEOF(); i++ {
		var piece []byte
		if d.wantsInput() {
			piece = frame[pos:min(pos+997, len(frame))]
			if len(piece) == 0 {
				break
			}
		}
		pos += len(piece)
		out, err := d.decompress(piece, 1001)
		if err != nil {
			res.Class, res.Msg = pyClass(err), err.Error()
			break
		}
		got = append(got, out...)
	}
	o := dataOutcome(got)
	res.SHA, res.Size, res.EOF = o.SHA, o.Size, d.isEOF()
	return res
}

// Damaged fuzz.py frames decode to the CPython 3.14 bytes or errors one shot, trickled and through zipfile
func TestDamagedFramesMatchCPython(t *testing.T) {
	blob, err := os.ReadFile(filepath.Join("testdata", "stream", "fuzz_bases.bin"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "stream", "fuzz.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Bases []struct {
			Offset, Length, Size int
			Crc                  uint32
		} `json:"bases"`
		Cases []fuzzCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) < 1000 {
		t.Fatalf("%d cases", len(doc.Cases))
	}
	dir := t.TempDir()
	a, z := []byte("leading stored member\n"), []byte("trailing stored member\n")
	for k, c := range doc.Cases {
		b := doc.Bases[c.Base]
		frame := applyDamage(blob[b.Offset:b.Offset+b.Length], c.Damage)
		name := fmt.Sprintf("c%04d", k)
		t.Run(name, func(t *testing.T) {
			d := newZstdDecompressor()
			out, err := d.decompress(frame, -1)
			got := dataOutcome(out)
			if err != nil {
				got = errOutcome(err)
			}
			if !reflect.DeepEqual(got, c.Oneshot) {
				t.Errorf("oneshot %+v, want %+v", got, c.Oneshot)
			}
			if tr := trickleDecode(frame); tr != c.Trickle {
				t.Errorf("trickle %+v, want %+v", tr, c.Trickle)
			}
			path := filepath.Join(dir, name+".zip")
			members := [][5]any{{"a.txt", 0, a, crc32.ChecksumIEEE(a), len(a)}, {"d/m.bin", 93, frame, b.Crc, b.Size}, {"z.txt", 0, z, crc32.ChecksumIEEE(z), len(z)}}
			if err := os.WriteFile(path, zipBytes(members), 0o644); err != nil {
				t.Fatal(err)
			}
			checkStreamArchive(t, path, c.Zip)
		})
	}
}
