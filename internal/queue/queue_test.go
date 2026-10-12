package queue

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeCodex writes a script that records argv and CODEX_HOME, then exits with code.
func fakeCodex(t *testing.T, body string) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls.log")
	bin = filepath.Join(dir, "codex")
	script := "#!/bin/sh\nprintf 'HOME=%s\\n' \"$CODEX_HOME\" >> '" + log + "'\nfor a in \"$@\"; do printf 'ARG=%s\\n' \"$a\" >> '" + log + "'; done\n" + body
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func TestSendPassesArgvAndHome(t *testing.T) {
	bin, log := fakeCodex(t, "echo queued\nexit 0\n")
	out, err := CLI{Bin: bin, Timeout: 5 * time.Second}.Send(context.Background(), "/tmp/h o me", "thread-1", "继续; rm -rf $HOME")
	if err != nil {
		t.Fatal(err)
	}
	if out != "queued" {
		t.Errorf("output %q", out)
	}
	b, _ := os.ReadFile(log)
	want := "HOME=/tmp/h o me\nARG=queue\nARG=--thread\nARG=thread-1\nARG=--message\nARG=继续; rm -rf $HOME\n"
	if string(b) != want {
		t.Errorf("calls:\n%s\nwant:\n%s", b, want)
	}
}

func TestSendFailure(t *testing.T) {
	bin, _ := fakeCodex(t, "echo 'no such thread' >&2\nexit 3\n")
	out, err := CLI{Bin: bin, Timeout: 5 * time.Second}.Send(context.Background(), "/h", "t", "继续")
	if err == nil || !strings.Contains(out, "no such thread") {
		t.Fatalf("err=%v out=%q", err, out)
	}
}

func TestSendTimeout(t *testing.T) {
	bin, _ := fakeCodex(t, "exec sleep 5\n")
	_, err := CLI{Bin: bin, Timeout: 200 * time.Millisecond}.Send(context.Background(), "/h", "t", "继续")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err=%v", err)
	}
}

func TestRejectsFlagLikeThread(t *testing.T) {
	if _, err := (CLI{Bin: "/bin/false", Timeout: time.Second}).Send(context.Background(), "/h", "--help", "x"); err == nil {
		t.Fatal("expected rejection")
	}
}
