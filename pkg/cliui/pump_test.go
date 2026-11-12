package cliui

import (
	"errors"
	"os"
	"testing"
	"time"
)

func TestInputAfterAConsoleReadStaysForTheNextReaderOfTheStream(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		write.Close()
		read.Close()
	})
	c := &Console{In: read}
	if _, err := write.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	if b, err := c.readByte(5*time.Second, nil); err != nil || b != 'a' {
		t.Fatalf("console read = %q, %v", b, err)
	}
	if _, err := c.readByte(50*time.Millisecond, nil); !errors.Is(err, errTimeout) {
		t.Fatalf("idle console read = %v", err)
	}
	if _, err := write.Write([]byte("bcd")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if err := read.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	direct := make([]byte, 8)
	n, err := read.Read(direct)
	if err != nil || string(direct[:n]) != "bcd" {
		t.Fatalf("direct read = %q, %v", direct[:n], err)
	}
	if _, err := write.Write([]byte("e")); err != nil {
		t.Fatal(err)
	}
	if b, err := c.readByte(5*time.Second, nil); err != nil || b != 'e' {
		t.Fatalf("console read after the direct reader = %q, %v", b, err)
	}
}
