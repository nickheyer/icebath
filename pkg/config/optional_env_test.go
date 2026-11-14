package config

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"sync"
	"testing"
)

func TestOptionalEnvDeclarationMatchesTheUpstreamTable(t *testing.T) {
	raw, err := os.ReadFile("testdata/optional_env_vars.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Prompt      string   `json:"prompt"`
		URL         *string  `json:"url"`
		Help        *string  `json:"help"`
		Password    *bool    `json:"password"`
		Category    string   `json:"category"`
		Tools       []string `json:"tools"`
		Advanced    bool     `json:"advanced"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 150 || len(optionalEnvDecl) != len(rows) {
		t.Fatalf("upstream rows %d, declared %d", len(rows), len(optionalEnvDecl))
	}
	text := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	for i, row := range rows {
		want := EnvVar{Description: row.Description, Prompt: row.Prompt, URL: text(row.URL), Help: text(row.Help),
			Password: row.Password != nil && *row.Password, Category: row.Category, Tools: row.Tools, Advanced: row.Advanced,
			PasswordOmitted: row.Password == nil}
		if got := optionalEnvDecl[i]; got.Name != row.Name || !reflect.DeepEqual(got.Entry, want) {
			t.Fatalf("row %d\n got %s %+v\nwant %s %+v", i, got.Name, got.Entry, row.Name, want)
		}
	}
	table := NewOptionalEnvTable(optionalEnvDecl)
	for _, absent := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "NOUS_API_KEY", "GROQ_API_KEY", "TOGETHER_API_KEY"} {
		if _, ok := table.RawGet(absent); ok {
			t.Fatalf("%s comes from a provider profile, never from the declaration", absent)
		}
	}
	browser, _ := table.RawGet("BROWSERBASE_API_KEY")
	if browser.Category != "tool" || !browser.Password || !slices.Equal(browser.Tools, []string{"browser_exec"}) ||
		browser.Description != "Browserbase API key for cloud browser (optional — local browser works without this)" {
		t.Fatalf("browserbase entry %+v", browser)
	}
	if e, _ := table.RawGet("ICEBATH_ANON_API_SECRET"); e.Category != "provider" || !e.Advanced || !e.Password {
		t.Fatalf("renamed entry %+v", e)
	}
	for name := range StaticCoreEnvNames() {
		if _, ok := table.RawGet(name); !ok {
			t.Fatalf("static core name %s is not declared", name)
		}
	}
	if len(StaticCoreEnvNames()) != 150 {
		t.Fatalf("static core names %d", len(StaticCoreEnvNames()))
	}
}

func TestOptionalEnvTableFillsOnceBeforeItsFirstReadAndNeverOnWrites(t *testing.T) {
	table := NewOptionalEnvTable([]NamedEnvVar{{"A", EnvVar{Description: "a"}}, {"B", EnvVar{Description: "b"}}})
	runs, published := 0, [][]NamedEnvVar{}
	var inside []string
	table.Arm(func() []NamedEnvVar {
		runs++
		inside = table.Names()
		if table.Has("C") || table.Len() != 3 {
			t.Error("a read made by the fill sees the table without its entries")
		}
		return []NamedEnvVar{{"B", EnvVar{Description: "filled b"}}, {"C", EnvVar{Description: "c"}}}
	}, func(entries []NamedEnvVar) { published = append(published, entries) })
	table.Set("LATE", EnvVar{})
	table.Delete("MISSING")
	if _, ok := table.RawGet("C"); ok || !slices.Equal(table.RawNames(), []string{"A", "B", "LATE"}) || runs != 0 {
		t.Fatalf("raw access and writes never fill, runs %d", runs)
	}
	if !table.Has("C") || runs != 1 || !slices.Equal(inside, []string{"A", "B", "LATE"}) {
		t.Fatalf("first read runs %d inside %v", runs, inside)
	}
	if got := table.Names(); !slices.Equal(got, []string{"A", "B", "LATE", "C"}) || table.Entry("B").Description != "filled b" || table.Len() != 4 {
		t.Fatalf("names %v b %+v", got, table.Entry("B"))
	}
	if copied := table.Copy(); len(copied) != 4 || copied["C"].Description != "c" || len(table.Items()) != 4 || runs != 1 || len(published) != 1 || len(published[0]) != 2 {
		t.Fatalf("copy %v runs %d published %v", copied, runs, published)
	}
	table.Delete("B")
	if got := table.Names(); !slices.Equal(got, []string{"A", "LATE", "C"}) {
		t.Fatalf("names after a delete %v", got)
	}
}

func TestOptionalEnvTableKeepsTheFillArmedWhenItPanicsAndRestoresSnapshots(t *testing.T) {
	table := NewOptionalEnvTable([]NamedEnvVar{{"A", EnvVar{}}})
	fail := true
	table.Arm(func() []NamedEnvVar {
		if fail {
			panic("provider layer down")
		}
		return []NamedEnvVar{{"P", EnvVar{Description: "p"}}}
	}, nil)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the failure of a fill reaches the reader")
			}
		}()
		table.Has("A")
	}()
	if got := table.RawNames(); !slices.Equal(got, []string{"A"}) {
		t.Fatalf("a failed fill publishes nothing, names %v", got)
	}
	saved := table.Snapshot()
	fail = false
	if !table.Has("P") {
		t.Fatal("the fill stays armed after a failure and runs on the next read")
	}
	table.Restore(saved)
	if got := table.RawNames(); !slices.Equal(got, []string{"A"}) {
		t.Fatalf("restored names %v", got)
	}
	if !table.Has("P") {
		t.Fatal("a restored snapshot carries its armed fill")
	}
	table.Arm(func() []NamedEnvVar { return []NamedEnvVar{{"Q", EnvVar{}}} }, nil)
	table.Arm(nil, nil)
	if table.Has("Q") {
		t.Fatal("a disarmed table runs no fill")
	}
}

func TestOptionalEnvTableConcurrentFirstReadersPublishOnce(t *testing.T) {
	table := NewOptionalEnvTable(nil)
	var mu sync.Mutex
	runs, publishes := 0, 0
	table.Arm(func() []NamedEnvVar {
		mu.Lock()
		runs++
		mu.Unlock()
		return []NamedEnvVar{{"P", EnvVar{Description: "p"}}}
	}, func([]NamedEnvVar) {
		mu.Lock()
		publishes++
		mu.Unlock()
	})
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !table.Has("P") {
				t.Error("every first reader sees the published entries")
			}
		}()
	}
	wg.Wait()
	if publishes != 1 || runs < 1 || !slices.Equal(table.Names(), []string{"P"}) {
		t.Fatalf("runs %d publishes %d names %v", runs, publishes, table.Names())
	}
}
