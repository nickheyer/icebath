package pydantic

import (
	"math"
	"os"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	os.Exit(m.Run())
}

var (
	closed  = Config{Extra: ExtraForbid}
	session = NewModel("S", closed, Req("type", Literal("session")), Req("session_id", ConStr(MinLen(1))))
	account = NewModel("A", closed, Req("type", Literal("account")))
	letters = Enum("E", "a", "a", "b", "b", "c", "c")
	probe   = NewModel("U", closed,
		Def("u1", Nullable(Union(Int, Str, List(Str), Dict(Int), session, Literal("q"), letters)), nil),
		Def("u2", Tagged("type", session, account), nil),
		Fac("l", List(Str, MaxLen(2)), NewList),
		Def("s", ConStr(MinLen(2)), ""),
		Def("pat", ConStr(Pattern(`^[a-z]+$`)), "a"),
		Def("i", Int, 0),
		Def("f", Float, 0),
		Def("b", Bool, false),
	)
)

// Locations and messages of every problem, one line each
func problems(err *ValidationError) string {
	lines := []string{}
	for _, e := range err.Errors() {
		lines = append(lines, e.LocText()+" "+e.Type+" "+e.Msg)
	}
	return strings.Join(lines, "\n")
}

func TestUnionMembersReportUnderPydanticLabels(t *testing.T) {
	_, err := probe.Validate(pyjson.ObjectOf("u1", 1.5, "u2", pyjson.ObjectOf("type", "zzz"), "l", []any{"a", "b", "c"}, "s", "a", "pat", "A1", "i", true, "f", "x", "b", 2))
	want := `u1.int int_from_float Input should be a valid integer, got a number with a fractional part
u1.str string_type Input should be a valid string
u1.list[str] list_type Input should be a valid list
u1.dict[str,int] dict_type Input should be a valid dictionary
u1.S model_type Input should be a valid dictionary or instance of S
u1.literal['q'] literal_error Input should be 'q'
u1.str-enum[E] enum Input should be 'a', 'b' or 'c'
u2 union_tag_invalid Input tag 'zzz' found using 'type' does not match any of the expected tags: 'session', 'account'
l too_long List should have at most 2 items after validation, not 3
s string_too_short String should have at least 2 characters
pat string_pattern_mismatch String should match pattern '^[a-z]+$'
f float_parsing Input should be a valid number, unable to parse string as a number
b bool_parsing Input should be a valid boolean, unable to interpret input`
	if err == nil || problems(err) != want {
		t.Fatalf("problems\n%v", err)
	}
}

func TestTaggedUnionRefusesUntaggedAndNonMappingInput(t *testing.T) {
	_, err := probe.Validate(map[string]any{"u2": map[string]any{"session_id": "x"}})
	if err == nil || problems(err) != "u2 union_tag_not_found Unable to extract tag using discriminator 'type'" {
		t.Fatalf("untagged %v", err)
	}
	_, err = probe.Validate(map[string]any{"u2": "str"})
	if err == nil || problems(err) != "u2 model_attributes_type Input should be a valid dictionary or object to extract fields from" {
		t.Fatalf("string %v", err)
	}
	_, err = probe.Validate(map[string]any{"u2": map[string]any{"type": "session", "session_id": ""}})
	if err == nil || problems(err) != "u2.session.session_id string_too_short String should have at least 1 character" {
		t.Fatalf("tagged member %v", err)
	}
}

func TestErrorTextMatchesPydantic(t *testing.T) {
	_, err := NewModel("P", Config{Extra: ExtraForbid, PopulateByName: true}, Req("req", Str).Alias("_req")).Validate([]any{1})
	want := "1 validation error for P\n  Input should be a valid dictionary or instance of P [type=model_type, input_value=[1], input_type=list]\n    For further information visit https://errors.pydantic.dev/2.14/v/model_type"
	if err == nil || err.Error() != want {
		t.Fatalf("text %v", err)
	}
}

func TestAliasLookupLeavesTheUnusedNameAnExtraKey(t *testing.T) {
	p := NewModel("P", Config{Extra: ExtraForbid, PopulateByName: true},
		Def("x", Nullable(Int), nil).Alias("_x"), Req("req", Str).Alias("_req"))
	_, err := p.Validate(pyjson.ObjectOf("x", 1))
	if err == nil || problems(err) != "_req missing Field required" {
		t.Fatalf("missing %v", err)
	}
	_, err = p.Validate(pyjson.ObjectOf("req", "a", "x", "1", "_x", 2, "y", 3))
	if err == nil || problems(err) != "x extra_forbidden Extra inputs are not permitted\ny extra_forbidden Extra inputs are not permitted" {
		t.Fatalf("extras %v", err)
	}
}

func TestScalarCoercionsFollowPydanticCore(t *testing.T) {
	m := NewModel("V", Config{}, Def("i", Nullable(Int), nil), Def("f", Nullable(Float), nil), Def("b", Nullable(Bool), nil))
	cases := []struct {
		field string
		in    any
		want  any
	}{
		{"i", " 1 ", int64(1)}, {"i", "1.000", int64(1)}, {"i", "1_0", int64(10)}, {"i", "1.", "int_parsing"}, {"i", "-_1", "int_parsing"},
		{"i", 2.0, int64(2)}, {"i", math.NaN(), "finite_number"}, {"i", 1e30, "int_parsing_size"}, {"i", true, int64(1)},
		{"f", "1_0.5", 10.5}, {"f", "-_1", -1.0}, {"f", " 1_000 ", "float_parsing"}, {"f", "0x10", "float_parsing"}, {"f", true, 1.0},
		{"b", "On", true}, {"b", " true", "bool_parsing"}, {"b", 2, "bool_parsing"}, {"b", 2.5, "bool_type"}, {"b", 0.0, false},
	}
	for _, c := range cases {
		inst, err := m.Validate(map[string]any{c.field: c.in})
		var got any
		if err != nil {
			got = err.Errors()[0].Type
		} else {
			got = inst.Get(c.field)
		}
		if got != c.want {
			t.Errorf("%s %#v gave %#v, want %#v", c.field, c.in, got, c.want)
		}
	}
}

func TestSmartUnionPrefersTheExactModel(t *testing.T) {
	m1 := NewModel("M1", closed, Def("a", Int, 0))
	m2 := NewModel("M2", closed, Def("a", Str, ""), Def("b", Int, 0))
	u := NewModel("U", closed, Def("u", Nullable(Union(m1, m2)), nil))
	for _, c := range []struct {
		in   *pyjson.Object
		want string
	}{
		{pyjson.ObjectOf("a", "1"), "M2(a='1', b=0)"},
		{pyjson.ObjectOf("a", 1), "M1(a=1)"},
		{pyjson.ObjectOf(), "M1(a=0)"},
		{pyjson.ObjectOf("b", 1), "M2(a='', b=1)"},
	} {
		inst, err := u.Validate(pyjson.ObjectOf("u", c.in))
		if err != nil {
			t.Fatal(err)
		}
		if got := inst.Get("u").(*Instance).PyRepr(); got != c.want {
			t.Errorf("%v chose %s, want %s", c.in.Keys, got, c.want)
		}
	}
}

func TestDumpJSONWritesFloatsLikePydanticCore(t *testing.T) {
	m := NewModel("F", closed, Req("f", List(Float)), Def("a", Any, nil))
	inst, err := m.Validate(map[string]any{
		"f": []any{1e20, 1e16, 1e15, 1234567890123456.7, 0.0001, 1e-5, 1.5, math.Copysign(0, -1), 123.456, 5e-324, 1.7976931348623157e308, math.Inf(1), 2.5e-7, 1e21, 12345e30},
		"a": map[string]any{"x": []any{1e22, 0.5}}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"f":[1e+20,1e+16,1000000000000000.0,1234567890123456.8,0.0001,0.00001,1.5,-0.0,123.456,5e-324,1.7976931348623157e+308,null,2.5e-7,1e+21,1.2345e+34],"a":{"x":[1e+22,0.5]}}`
	if got := inst.DumpJSON(DumpOptions{}); got != want {
		t.Fatalf("json %s", got)
	}
}
