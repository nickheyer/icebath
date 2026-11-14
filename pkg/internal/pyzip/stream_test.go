package pyzip

import (
	"reflect"
	"testing"
)

// Block sizes grow 32 KiB, 64 KiB, 256 KiB and on, the last block cut at max_length
func TestOutputBufferGrowsLikeBlocksOutputBuffer(t *testing.T) {
	sizes := func(maxLength, blocks int) []int {
		b := newOutputBuffer(maxLength)
		got := []int{len(b.data)}
		for len(got) < blocks && (maxLength < 0 || len(b.data) < maxLength) {
			b.grow()
			got = append(got, len(b.data))
		}
		return got
	}
	if got := sizes(-1, 5); !reflect.DeepEqual(got, []int{32 << 10, 96 << 10, 352 << 10, 352<<10 + 1<<20, 352<<10 + 5<<20}) {
		t.Fatalf("unbounded %v", got)
	}
	if got := sizes(100<<10, 5); !reflect.DeepEqual(got, []int{32 << 10, 96 << 10, 100 << 10}) {
		t.Fatalf("bounded %v", got)
	}
	if got := sizes(4096, 5); !reflect.DeepEqual(got, []int{4096}) {
		t.Fatalf("small %v", got)
	}
}

// Illegal characters become underscores and parts lose trailing dots and spaces
func TestSanitizeWindowsName(t *testing.T) {
	cases := map[string]string{`a:b<c>d|e"f?g*h`: "a_b_c_d_e_f_g_h", `dir. \file .`: `dir\file`, `x\\y`: `x\y`, ` . `: ""}
	for in, want := range cases {
		if got := sanitizeWindowsName(in, `\`); got != want {
			t.Fatalf("%q: %q want %q", in, got, want)
		}
	}
}

type sinkWriter []byte

func (w *sinkWriter) Write(p []byte) (int, error) {
	*w = append(*w, p...)
	return len(p), nil
}
