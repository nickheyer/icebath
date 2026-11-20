package media

import (
	"slices"
	"testing"
)

func TestSentenceChunkerCutsSentenceTheMomentItsBoundaryArrives(t *testing.T) {
	c := NewSentenceChunker(20)
	if got := c.Feed("This is the first full"); len(got) != 0 {
		t.Fatalf("%q", got)
	}
	if got := c.Feed(" sentence of it all. And"); !slices.Equal(got, []string{"This is the first full sentence of it all. "}) {
		t.Fatalf("%q", got)
	}
	if got := c.Flush(); !slices.Equal(got, []string{"And"}) {
		t.Fatalf("%q", got)
	}
}

func TestSentenceChunkerThinkBlocksAreStrippedEvenAcrossDeltas(t *testing.T) {
	c := NewSentenceChunker(20)
	if got := c.Feed("<think>secret reason"); len(got) != 0 {
		t.Fatalf("%q", got)
	}
	if got := c.Feed("ing</think>The actual spoken answer. "); !slices.Equal(got, []string{"The actual spoken answer. "}) {
		t.Fatalf("%q", got)
	}
}

func TestSentenceChunkerParagraphBreakIsABoundary(t *testing.T) {
	c := NewSentenceChunker(20)
	if got := c.Feed("A paragraph without punctuation\n\nnext one"); !slices.Equal(got, []string{"A paragraph without punctuation\n\n"}) {
		t.Fatalf("%q", got)
	}
}

func TestFlushDropsUnterminatedThinkTail(t *testing.T) {
	for _, tag := range []string{"<think>", "<thinking>", "<THINK>", "<reasoning>"} {
		c := NewSentenceChunker(20)
		if got := c.Feed("The spoken part. " + tag + "half-formed reas"); len(got) != 0 {
			t.Fatalf("%s feed %q", tag, got)
		}
		if got := c.Flush(); !slices.Equal(got, []string{"The spoken part."}) {
			t.Fatalf("%s flush %q", tag, got)
		}
	}
}
