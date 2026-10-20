package fx

import (
	"testing"
	"unicode/utf8"
)

func TestDecodeFullyRevealed(t *testing.T) {
	if got := Decode("UPLINK", 1, 3); got != "UPLINK" {
		t.Fatalf("got %q", got)
	}
	if got := Decode("UPLINK", 1.7, 3); got != "UPLINK" {
		t.Fatalf("got %q", got)
	}
}

func TestDecodeKeepsShapeWhileScrambled(t *testing.T) {
	got := Decode("ICE WALL", 0, 9)
	if utf8.RuneCountInString(got) != 8 || []rune(got)[3] != ' ' {
		t.Fatalf("got %q", got)
	}
	if got == "ICE WALL" {
		t.Fatal("nothing scrambled")
	}
	if Decode("ICE WALL", 0, 9) != got {
		t.Fatal("same tick must scramble identically")
	}
	if Decode("ICE WALL", 0, 10) == got {
		t.Fatal("different ticks must churn")
	}
}

func TestDecodePartial(t *testing.T) {
	got := []rune(Decode("BLACKWALL", 0.5, 1))
	if string(got[:4]) != "BLAC" {
		t.Fatalf("got %q", string(got))
	}
	if Revealed("BLACKWALL", 0.5) != 4 || Revealed("BLACKWALL", -1) != 0 || Revealed("", 0.5) != 0 {
		t.Fatal("reveal count")
	}
}
