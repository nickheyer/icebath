package pyurl

import (
	"reflect"
	"testing"
)

func TestParseQSMatchesCPython(t *testing.T) {
	cases := []struct {
		qs   string
		keep bool
		want map[string][]string
	}{
		{"socket=gateway", false, map[string][]string{"socket": {"gateway"}}},
		{"socket=gateway&socket=x", false, map[string][]string{"socket": {"gateway", "x"}}},
		{"a=&b&&c=d+e%21", false, map[string][]string{"c": {"d e!"}}},
		{"a=&b&&c=d+e%21", true, map[string][]string{"a": {""}, "b": {""}, "c": {"d e!"}}},
		{"x%3Dy=1=2;z=3", false, map[string][]string{"x=y": {"1=2;z=3"}}},
		{"", false, map[string][]string{}},
	}
	for _, c := range cases {
		if got := ParseQS(c.qs, c.keep); !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParseQS(%q, %v) = %v want %v", c.qs, c.keep, got, c.want)
		}
	}
}
