package skills

import (
	"reflect"
	"testing"
)

func TestSlugifySkillName(t *testing.T) {
	for in, want := range map[string]string{"My Skill_Name": "my-skill-name", "c++/tools": "ctools", "--a  b--": "a-b", "Ünïcode Skill": "ünïcode-skill"} {
		if got := SlugifySkillName(in); got != want {
			t.Errorf("SlugifySkillName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSkillsLoadedByScaffold(t *testing.T) {
	single := skillInvocationPrefix + `"demo" skill, indicating they want you to follow its instructions. ` + singleSkillMarker + "\n\nbody"
	if got := SkillsLoadedByScaffold(single); !reflect.DeepEqual(got, []string{"demo"}) {
		t.Fatalf("single %v", got)
	}
	bundle := skillInvocationPrefix + `"ops"` + bundleMarker + " x]\nSkills loaded: a, b ,\n"
	if got := SkillsLoadedByScaffold(bundle); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("bundle %v", got)
	}
	auto := autoLoadPrefix + `one" skill is auto-loaded. Follow its instructions for this session.]` + "\n" + autoLoadPrefix + `two" skill is auto-loaded. Follow its instructions for this session.]`
	if got := SkillsLoadedByScaffold(auto); !reflect.DeepEqual(got, []string{"one", "two"}) {
		t.Fatalf("auto %v", got)
	}
	if got := SkillsLoadedByScaffold(autoLoadPrefix + `config" file is broken]`); got != nil {
		t.Fatalf("typed lookalike %v", got)
	}
}

func TestMissingSkillFormatting(t *testing.T) {
	label, ok := AmbiguousSkillLabel("dup", map[string]any{"load_names": []any{"a/dup", "b/dup"}})
	if !ok || label != AmbiguousSkillPrefix+"dup: use one of a/dup, b/dup" {
		t.Fatalf("label %q", label)
	}
	if _, ok := AmbiguousSkillLabel("x", map[string]any{"error": "nope"}); ok {
		t.Fatal("non ambiguous payload labelled")
	}
	if got := FormatMissingSkills([]string{"x", label, "y"}); got != label+"; Unknown skill(s): x, y" {
		t.Fatalf("format %q", got)
	}
}

func TestExtractUserInstructionDelegates(t *testing.T) {
	msg := skillInvocationPrefix + `"demo" skill, x. ` + singleSkillMarker + "\n\nbody\n\n" + singleSkillInstruction + "do it"
	got, ok := ExtractUserInstructionFromSkillMessage(msg)
	if !ok || got != "do it" {
		t.Fatalf("instruction %q %v", got, ok)
	}
}
