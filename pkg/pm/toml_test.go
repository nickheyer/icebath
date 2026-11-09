package pm

import "testing"

func TestTOMLLoadsExport(t *testing.T) {
	doc, err := TOMLLoads("[project]\nversion = \"0.21.4\"\n")
	if err != nil {
		t.Fatal(err)
	}
	project, ok := doc.Get("project")
	if !ok {
		t.Fatal("project table missing")
	}
	version, _ := project.(*OrderedObject).Get("version")
	if version != "0.21.4" {
		t.Fatalf("version %v", version)
	}
	if _, err := TOMLLoads("= broken"); err == nil {
		t.Fatal("invalid document accepted")
	}
}
