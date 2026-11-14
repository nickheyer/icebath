package tools

import (
	"bytes"
	"context"
	"math/rand"
	"reflect"
	"slices"
	"testing"
)

type fixedSource int64

func (s fixedSource) Int63() int64 { return int64(s) }
func (fixedSource) Seed(int64)     {}

var rollHit = rand.New(fixedSource(0))
var rollMiss = rand.New(fixedSource(9131138316486228049))

func TestListDistributionsReturnsCopy(t *testing.T) {
	d1, d2 := ListDistributions(), ListDistributions()
	if reflect.ValueOf(d1).Pointer() == reflect.ValueOf(d2).Pointer() || !reflect.DeepEqual(d1, d2) {
		t.Fatal("ListDistributions must return equal distinct copies")
	}
}

func TestDistributionStructure(t *testing.T) {
	for name, d := range Distributions {
		if d.Description == "" || d.Toolsets == nil || len(d.Order) != len(d.Toolsets) {
			t.Fatalf("%s missing description or toolsets", name)
		}
		for entry, p := range d.Toolsets {
			if p <= 0 || p > 100 {
				t.Fatalf("%s.%s has invalid probability %d", name, entry, p)
			}
		}
	}
}

func withDistribution(t *testing.T, name string, d Distribution) {
	Distributions[name] = d
	t.Cleanup(func() { delete(Distributions, name) })
}

func TestGroupedCompoundEntries(t *testing.T) {
	ctx := context.Background()
	got, _, err := Default.SampleToolsetsFromDistribution(ctx, "", "browser_tasks", rollHit)
	if err != nil || !slices.Contains(got, "browser") || !slices.Contains(got, "search") || slices.Contains(got, "browser+search") {
		t.Fatalf("hit roll selected %v, %v", got, err)
	}
	withDistribution(t, "_test_compound_fallback", dist("t", "browser+search", 60, "web", 10))
	got, _, _ = Default.SampleToolsetsFromDistribution(ctx, "", "_test_compound_fallback", rollMiss)
	slices.Sort(got)
	if !reflect.DeepEqual(got, []string{"browser", "search"}) {
		t.Fatalf("fallback selected %v", got)
	}
	withDistribution(t, "_test_compound_invalid", dist("t", "browser+__not_a_toolset__", 100, "web", 100))
	got, warnings, _ := Default.SampleToolsetsFromDistribution(ctx, "", "_test_compound_invalid", rollHit)
	if slices.Contains(got, "browser") || !slices.Contains(got, "web") {
		t.Fatalf("invalid member did not skip its group: %v", got)
	}
	if len(warnings) != 1 || warnings[0] != "Toolset '__not_a_toolset__' in distribution '_test_compound_invalid' is not valid" {
		t.Fatalf("warnings %v", warnings)
	}
	withDistribution(t, "_test_tie", dist("t", "web", 50, "file", 50))
	if got, _, _ := Default.SampleToolsetsFromDistribution(ctx, "", "_test_tie", rollMiss); !reflect.DeepEqual(got, []string{"web"}) {
		t.Fatalf("fallback tie must pick the first declared entry: %v", got)
	}
}

func TestPrintDistributionInfo(t *testing.T) {
	var b bytes.Buffer
	PrintDistributionInfo(&b, "research")
	want := "\n📊 Distribution: research\n   Description: Web research with vision analysis and reasoning\n   Toolsets:\n" +
		"     • web             :  90% chance\n     • browser         :  70% chance\n     • vision          :  50% chance\n     • terminal        :  10% chance\n"
	if b.String() != want {
		t.Fatalf("got %q", b.String())
	}
	b.Reset()
	PrintDistributionInfo(&b, "default")
	if want := "     • web             : 100% chance\n     • vision          : 100% chance\n"; !bytes.Contains(b.Bytes(), []byte(want)) {
		t.Fatalf("ties must keep declaration order: %q", b.String())
	}
	b.Reset()
	PrintDistributionInfo(&b, "nope")
	if b.String() != "❌ Unknown distribution: nope\n" {
		t.Fatalf("got %q", b.String())
	}
}
