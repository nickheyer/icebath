package proc

import (
	"reflect"
	"testing"
)

func TestParseCmdlineFollowsPsutil(t *testing.T) {
	cases := []struct {
		data string
		want []string
	}{
		{"python\x00-m\x00pytest\x00", []string{"python", "-m", "pytest"}},
		{"arg with space\x00next\x00", []string{"arg with space", "next"}},
		{"chrome --type=renderer --lang", []string{"chrome", "--type=renderer", "--lang"}},
		{"retitled worker \x00", []string{"retitled", "worker", ""}},
		{"title ", []string{"title"}},
		{"\x00", []string{""}},
		{"", []string{}},
	}
	for _, c := range cases {
		if got := parseCmdline(c.data); !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseCmdline(%q) = %q, want %q", c.data, got, c.want)
		}
	}
}
