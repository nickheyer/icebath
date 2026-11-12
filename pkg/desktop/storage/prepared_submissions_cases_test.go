package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func journalAt(dir, origin string) *PreparedJournal {
	return NewPreparedJournal(dir, origin, func(string) {})
}

func jsonValue(t *testing.T, text string) any {
	t.Helper()
	v, err := jsParse(text)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestAcknowledgedJournalMutationsSurviveReopeningWithExactPayloadAndOriginIsolation(t *testing.T) {
	dir := t.TempDir()
	origin := "http://localhost:5174"
	key := `["remote","profile-a","stored-a"]`
	entryText := `{"id":"id-a","text":"Ω 👩🏽‍💻 e` + "́" + `\n  []","attachments":[{"id":"asset","path":"/tmp/Ω"}],"params":{"session_id":"live-a"}}`
	if err := journalAt(dir, origin).Update(key, jsonValue(t, entryText)); err != nil {
		t.Fatal(err)
	}
	if err := journalAt(dir, origin).Update("other-profile", jsonValue(t, `{"id":"id-b"}`)); err != nil {
		t.Fatal(err)
	}
	read, err := journalAt(dir, origin).Read()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := read.Get(key); jsStringify(got, "") != jsStringify(jsonValue(t, entryText), "") {
		t.Fatalf("entry %s", jsStringify(got, ""))
	}
	if other, _ := journalAt(dir, "http://other:5174").Read(); other.Len() != 0 {
		t.Fatalf("other origin %s", jsStringify(other, ""))
	}
	if err := journalAt(dir, origin).Update(key, nil); err != nil {
		t.Fatal(err)
	}
	if after, _ := journalAt(dir, origin).Read(); jsStringify(after, "") != `{"other-profile":{"id":"id-b"}}` {
		t.Fatalf("after %s", jsStringify(after, ""))
	}
}

func TestACorruptJournalIsQuarantinedWithItsBytesKeptAndTheOriginCanSendAgain(t *testing.T) {
	dir := t.TempDir()
	origin := "http://localhost:5174"
	if err := journalAt(dir, origin).Update("kept", jsonValue(t, `{"id":"id-a"}`)); err != nil {
		t.Fatal(err)
	}
	names, _ := os.ReadDir(dir)
	if len(names) != 1 {
		t.Fatalf("files %v", names)
	}
	if err := os.WriteFile(filepath.Join(dir, names[0].Name()), []byte(`{"kept":{"id":"id-`), 0o600); err != nil {
		t.Fatal(err)
	}
	if read, err := journalAt(dir, origin).Read(); err != nil || read.Len() != 0 {
		t.Fatalf("read %v %v", read, err)
	}
	if err := journalAt(dir, origin).Update("next", jsonValue(t, `{"id":"id-b"}`)); err != nil {
		t.Fatal(err)
	}
	if read, _ := journalAt(dir, origin).Read(); jsStringify(read, "") != `{"next":{"id":"id-b"}}` {
		t.Fatalf("read %s", jsStringify(read, ""))
	}
	var aside []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".corrupt-") {
			raw, _ := os.ReadFile(filepath.Join(dir, e.Name()))
			aside = append(aside, string(raw))
		}
	}
	if len(aside) != 1 || aside[0] != `{"kept":{"id":"id-` {
		t.Fatalf("aside %q", aside)
	}
}

func TestPreparedSubmissionCompareAndSetAppliesOnlyOverTheExpectedRecord(t *testing.T) {
	j := journalAt(t.TempDir(), "null")
	res, err := j.CompareAndSetText(json.RawMessage(`"k"`), json.RawMessage(`null`), json.RawMessage(`"{\"id\":1}"`))
	if err != nil || jsStringify(res, "") != `{"applied":true,"current":"{\"id\":1}"}` {
		t.Fatalf("create %s %v", jsStringify(res, ""), err)
	}
	res, _ = j.CompareAndSetText(json.RawMessage(`"k"`), json.RawMessage(`null`), json.RawMessage(`"{\"id\":2}"`))
	if jsStringify(res, "") != `{"applied":false,"current":"{\"id\":1}"}` {
		t.Fatalf("stale %s", jsStringify(res, ""))
	}
	if _, err := j.CompareAndSetText(json.RawMessage(`1`), json.RawMessage(`null`), json.RawMessage(`null`)); err == nil || err.Error() != "Invalid prepared submission comparison" {
		t.Fatalf("bad key %v", err)
	}
	if err := j.UpdateText(json.RawMessage(`"k"`), json.RawMessage(`{}`)); err == nil || err.Error() != "Invalid prepared submission" {
		t.Fatalf("bad entry %v", err)
	}
	if text, _ := j.ReadText(); text != `{"k":{"id":1}}` {
		t.Fatalf("read %s", text)
	}
	if URLOrigin("wails://localhost/") != "null" || URLOrigin("http://wails.localhost/") != "http://wails.localhost" || URLOrigin("http://LOCALHOST:80/x") != "http://localhost" {
		t.Fatal("origins")
	}
}
