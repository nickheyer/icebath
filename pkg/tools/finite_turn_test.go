package tools

import "testing"

func TestFiniteTurnFlagReadsTheRecordedFlag(t *testing.T) {
	if _, known := FiniteTurnFlag(nil); known {
		t.Fatal("nil env reports a known flag")
	}
	env := &Env{}
	if _, known := FiniteTurnFlag(env); known {
		t.Fatal("fresh env reports a known flag")
	}
	SetFiniteTurn(env, true)
	if finite, known := FiniteTurnFlag(env); !finite || !known {
		t.Fatalf("finite=%v known=%v after SetFiniteTurn(true)", finite, known)
	}
	SetFiniteTurn(env, false)
	if finite, known := FiniteTurnFlag(env); finite || !known {
		t.Fatalf("finite=%v known=%v after SetFiniteTurn(false)", finite, known)
	}
	if finite, known := FiniteTurnRequired(env); finite || !known {
		t.Fatalf("default hook finite=%v known=%v", finite, known)
	}
}
