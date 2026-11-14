package pm

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Security consumer store over a loopback range server with lazy installs off
func ieConsumerStore(t *testing.T) (*rangeServer, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixtures")
	}
	tmp := ieClientEnv(t)
	t.Setenv(EnvDisableLazyInstalls, "1")
	return newRangeServer(t), tmp
}

// Pins a consumer to a loopback archive of a shell fixture, with checksums and signature assets for iron-proxy
func iePinConsumer(t *testing.T, server *rangeServer, name, version string, badHash bool, payload []byte, signature []byte, link bool) {
	t.Helper()
	if payload == nil {
		payload = []byte("#!/bin/sh\necho '" + name + " fixture " + version + "'\n")
	}
	var buf bytes.Buffer
	ext := ".tar.gz"
	if name == "bws" {
		ext = ".zip"
		zw := zip.NewWriter(&buf)
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(0o600)
		w, _ := zw.CreateHeader(header)
		w.Write(payload)
		zw.Close()
	} else {
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		member := name
		if link {
			member = "payload"
		}
		tw.WriteHeader(&tar.Header{Name: member, Mode: 0o755, Size: int64(len(payload)), Typeflag: tar.TypeReg})
		tw.Write(payload)
		if link {
			tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: "payload"})
		}
		tw.Close()
		gz.Close()
	}
	archive := "/" + name + "-" + version + ext
	server.set(archive, buf.Bytes())
	digest := hexSHA256(buf.Bytes())
	sha := digest
	if badHash {
		sha = strings.Repeat("0", 64)
	}
	artifacts := []any{map[string]any{"url": server.url(archive), "sha256": sha}}
	if name != "bws" {
		checksumPath := "/" + name + "/" + version + "/checksums.txt"
		checksum := []byte(digest + "  " + archive[1:] + "\n")
		server.set(checksumPath, checksum)
		artifacts = append(artifacts, map[string]any{"url": server.url(checksumPath), "sha256": hexSHA256(checksum)})
		if signature != nil {
			for _, f := range []struct {
				name string
				raw  []byte
			}{{"checksums.txt.asc", signature}, {"public-key.asc", []byte("public-key")}} {
				path := "/" + name + "/" + version + "/" + f.name
				server.set(path, f.raw)
				artifacts = append(artifacts, map[string]any{"url": server.url(path), "sha256": hexSHA256(f.raw)})
			}
		}
	}
	target, _ := CurrentTarget()
	lock := NewLockfile(LockfilePath())
	lock.SetPin(name, version, map[string]any{target: artifacts})
	if err := lock.Save(); err != nil {
		t.Fatal(err)
	}
}

// The consumer lookup the bws and iron-proxy sources run, PATH first, then PM, then a lazy ensure
func ieFind(name string, installIfMissing bool) string {
	if system, err := exec.LookPath(name); err == nil {
		return system
	}
	ctx := context.Background()
	if selected, err := Installed(ctx, name, false); err == nil && selected != nil {
		return selected.Binary
	}
	if installIfMissing {
		if _, err := Ensure(ctx, name, EnsureOptions{}); err != nil {
			return ""
		}
		if selected, err := Installed(ctx, name, false); err == nil && selected != nil {
			return selected.Binary
		}
	}
	return ""
}

// The consumers' explicit install, re-verifying even a warm entry
func ieInstallConsumer(name string) (string, error) {
	ctx := context.Background()
	if _, err := Ensure(ctx, name, EnsureOptions{Explicit: true}); err != nil {
		return "", err
	}
	selected, err := Installed(ctx, name, false)
	if err != nil || selected == nil {
		return "", err
	}
	return selected.Binary, nil
}

// Output of a consumer binary
func ieRun(t *testing.T, binary string) string {
	t.Helper()
	out, err := exec.Command(binary).Output()
	if err != nil {
		t.Fatalf("%s: %v", binary, err)
	}
	return strings.TrimSpace(string(out))
}

// Binary an allow outdated lookup selects
func ieOutdatedBinary(t *testing.T, name string) string {
	t.Helper()
	selected, err := Installed(context.Background(), name, true)
	if err != nil || selected == nil {
		t.Fatalf("no selection for %s: %v", name, err)
	}
	return selected.Binary
}

func TestSecurityConsumerLifecycle(t *testing.T) {
	for _, name := range []string{"bws", "iron-proxy"} {
		t.Run(name, func(t *testing.T) {
			server, tmp := ieConsumerStore(t)
			iePinConsumer(t, server, name, "1", false, nil, nil, false)
			external := filepath.Join(tmp, name)
			writeFile(t, external, []byte("#!/bin/sh\necho external\n"), 0o755)
			t.Setenv("PATH", tmp)
			if got := ieFind(name, true); got != external || ieRun(t, external) != "external" {
				t.Fatalf("external lookup %q", got)
			}
			if selected, _ := Installed(context.Background(), name, false); selected != nil {
				t.Fatal("external tool counted as installed")
			}
			t.Setenv("PATH", "")
			if got := ieFind(name, true); got != "" || len(server.seen()) != 0 {
				t.Fatalf("lazy refusal fetched %q %v", got, server.seen())
			}
			binary, err := ieInstallConsumer(name)
			if err != nil || binary != ieFind(name, false) || ieRun(t, binary) != name+" fixture 1" {
				t.Fatalf("install %q %v", binary, err)
			}
			requests := server.seen()
			if again, _ := ieInstallConsumer(name); again != binary || len(server.seen()) != len(requests) {
				t.Fatal("a warm explicit install refetched")
			}
			os.WriteFile(binary, []byte("damaged executable"), 0o755)
			if again, err := ieInstallConsumer(name); err != nil || again != binary || ieRun(t, binary) != name+" fixture 1" {
				t.Fatalf("repair %v", err)
			}
			iePinConsumer(t, server, name, "2", true, nil, nil, false)
			if _, err := ieInstallConsumer(name); err == nil || !strings.Contains(strings.ToLower(err.Error()), "sha256") &&
				!strings.Contains(strings.ToLower(err.Error()), "hash") && !strings.Contains(strings.ToLower(err.Error()), "checksum") {
				t.Fatalf("bad hash got %v", err)
			}
			if ieOutdatedBinary(t, name) != binary || ieRun(t, binary) != name+" fixture 1" {
				t.Fatal("a rejected update lost the selection")
			}
			iePinConsumer(t, server, name, "2", false, nil, nil, false)
			updated, err := ieInstallConsumer(name)
			if err != nil || ieRun(t, updated) != name+" fixture 2" {
				t.Fatalf("update %v", err)
			}
		})
	}
}

func TestSecuritySignatureRejectionPreservesPreviousSelection(t *testing.T) {
	server, tmp := ieConsumerStore(t)
	commands := filepath.Join(tmp, "commands")
	log := filepath.Join(tmp, "verifier.jsonl")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + log + "'\nflag=''\nprev=''\nfor a in \"$@\"; do\n" +
		"  if [ \"$prev\" = --verify ]; then flag=\"$a\"; fi\n  prev=\"$a\"\ndone\n[ -z \"$flag\" ] && exit 0\n" +
		"[ \"$(cat \"$flag\")\" = accept ] && exit 0\nexit 1\n"
	writeFile(t, filepath.Join(commands, "gpg"), []byte(script), 0o755)
	t.Setenv("PATH", commands+":/usr/bin:/bin")
	iePinConsumer(t, server, "iron-proxy", "1", false, nil, []byte("accept"), false)
	old, err := ieInstallConsumer("iron-proxy")
	if err != nil {
		t.Fatal(err)
	}
	iePinConsumer(t, server, "iron-proxy", "2", false, nil, []byte("reject"), false)
	if _, err := ieInstallConsumer("iron-proxy"); err == nil || !strings.Contains(err.Error(), "GPG signature verification") {
		t.Fatalf("got %v", err)
	}
	if ieOutdatedBinary(t, "iron-proxy") != old || ieRun(t, old) != "iron-proxy fixture 1" {
		t.Fatal("a rejected signature lost the selection")
	}
	calls := strings.Split(strings.TrimSpace(string(readFile(t, log))), "\n")
	if !strings.Contains(calls[0], "--import") || !strings.Contains(calls[1], "--verify") {
		t.Fatalf("calls %v", calls)
	}
	fields := strings.Fields(calls[0])
	for i, field := range fields {
		if field == "--homedir" && pathExists(fields[i+1]) {
			t.Fatal("the verifier home outlived verification")
		}
	}
}

func TestSecurityNonregularSecurityBinaryIsNeverPublished(t *testing.T) {
	server, _ := ieConsumerStore(t)
	t.Setenv("PATH", "")
	iePinConsumer(t, server, "iron-proxy", "1", false, nil, nil, false)
	old, err := ieInstallConsumer("iron-proxy")
	if err != nil {
		t.Fatal(err)
	}
	iePinConsumer(t, server, "iron-proxy", "2", false, nil, nil, true)
	_, err = Ensure(context.Background(), "iron-proxy", EnsureOptions{Explicit: true})
	ieWantInstallError(t, err, "not a regular file")
	if ieOutdatedBinary(t, "iron-proxy") != old {
		t.Fatal("a link binary replaced the selection")
	}
}

func TestSecurityInterruptedConsumerDownloadCanRetryWithoutLosingSelection(t *testing.T) {
	server, _ := ieConsumerStore(t)
	t.Setenv("PATH", "")
	iePinConsumer(t, server, "bws", "1", false, nil, nil, false)
	old, err := ieInstallConsumer("bws")
	if err != nil {
		t.Fatal(err)
	}
	iePinConsumer(t, server, "bws", "2", false, nil, nil, false)
	server.mu.Lock()
	server.abortAfter = 0
	server.mu.Unlock()
	_, err = ieInstallConsumer("bws")
	server.mu.Lock()
	server.abortAfter = -1
	server.mu.Unlock()
	if !isInstallError(err) {
		t.Fatalf("got %v", err)
	}
	if ieOutdatedBinary(t, "bws") != old || ieRun(t, old) != "bws fixture 1" {
		t.Fatal("an interrupted download lost the selection")
	}
	if updated, err := ieInstallConsumer("bws"); err != nil || ieRun(t, updated) != "bws fixture 2" {
		t.Fatalf("retry %v", err)
	}
}

func TestSecurityLockedProvenanceIsRequiredEvenWithoutAVerifier(t *testing.T) {
	server, _ := ieConsumerStore(t)
	t.Setenv("PATH", "")
	iePinConsumer(t, server, "iron-proxy", "1", false, nil, []byte("pinned-signature"), false)
	old, err := ieInstallConsumer("iron-proxy")
	if err != nil {
		t.Fatal(err)
	}
	iePinConsumer(t, server, "iron-proxy", "2", false, nil, []byte("pinned-signature"), false)
	missing := "/iron-proxy/2/checksums.txt.asc"
	server.mu.Lock()
	raw := server.payloads[missing]
	delete(server.payloads, missing)
	server.mu.Unlock()
	if _, err := ieInstallConsumer("iron-proxy"); !isInstallError(err) {
		t.Fatalf("got %v", err)
	}
	if ieOutdatedBinary(t, "iron-proxy") != old {
		t.Fatal("missing provenance replaced the selection")
	}
	server.set(missing, raw)
}
