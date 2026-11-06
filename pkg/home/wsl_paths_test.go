package home

import "testing"

func TestWindowsDriveToWSLMount(t *testing.T) {
	for in, want := range map[string]string{`C:\Users\alex`: "/mnt/c/Users/alex", "C:/Users/alex": "/mnt/c/Users/alex", `D:\`: "/mnt/d/"} {
		if got, ok := WindowsPathToWSL(in); !ok || got != want {
			t.Fatalf("WindowsPathToWSL(%q) = %q, %v", in, got, ok)
		}
	}
	for _, in := range []string{"/home/alex", `relative\dir`} {
		if got, ok := WindowsPathToWSL(in); ok {
			t.Fatalf("WindowsPathToWSL(%q) = %q, want none", in, got)
		}
	}
}

func TestTranslateMapsWindowsAndUNCOnWSL(t *testing.T) {
	swap(t, &isWSL, func() bool { return true })
	for in, want := range map[string]string{`C:\Users\alex`: "/mnt/c/Users/alex", `\\wsl.localhost\Ubuntu\home\alex`: "/home/alex", `\\WSL$\Debian\srv`: "/srv", "/home/alex": "/home/alex"} {
		if got := TranslateCwdForWSLBackend(in); got != want {
			t.Fatalf("TranslateCwdForWSLBackend(%q) = %q, want %q", in, got, want)
		}
	}
	swap(t, &isWSL, func() bool { return false })
	if got := TranslateCwdForWSLBackend(`C:\Users\alex`); got != `C:\Users\alex` {
		t.Fatalf("off WSL the cwd changed to %q", got)
	}
}
