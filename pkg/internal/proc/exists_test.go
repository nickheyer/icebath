package proc

import (
	"os"
	"testing"
)

func TestExistsSeesSelfAndRejectsNonPositive(t *testing.T) {
	if !Exists(os.Getpid()) {
		t.Fatal("own process reported dead")
	}
	if Exists(0) || Exists(-1) {
		t.Fatal("non positive pid reported alive")
	}
}
