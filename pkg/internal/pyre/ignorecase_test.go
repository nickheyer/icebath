package pyre

import "testing"

func TestCompileIgnoreCaseMatchesAcrossCaseAndKeepsPythonErrors(t *testing.T) {
	re, err := CompileIgnoreCase(`\bhey\s+bot\b`)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := re.MatchString("well HEY  Bot there"); !ok {
		t.Fatal("case insensitive match missed")
	}
	if _, err := CompileIgnoreCase("("); err == nil || err.Error() != "missing ), unterminated subpattern at position 0" {
		t.Fatalf("err %v", err)
	}
}
