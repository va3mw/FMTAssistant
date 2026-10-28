package fmtdata

import (
	"os"
	"path/filepath"
	"testing"
)

func appendTo(t *testing.T, p, s string) {
	t.Helper()
	fh, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteString(s); err != nil {
		t.Fatal(err)
	}
	fh.Close()
}

func poll(t *testing.T, f *Follower) Update {
	t.Helper()
	u, err := f.Poll()
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestFollower(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "fmt.all")
	f := &Follower{Path: p}

	if u := poll(t, f); !u.Gone || u.Reset || len(u.Lines) != 0 {
		t.Fatalf("missing file: %+v", u)
	}
	appendTo(t, p, line(100, 7064, 1500, 1500.1, 30)+"\r\n"+line(106, 7064, 1500, 1500.2, 30)+"\r\n")
	if u := poll(t, f); u.Gone || u.Reset || len(u.Lines) != 2 || u.Lines[1].N != 2 {
		t.Fatalf("first read: %+v", u)
	}
	if u := poll(t, f); len(u.Lines) != 0 || u.Reset {
		t.Fatalf("nothing new: %+v", u)
	}

	// A line written in two parts is only reported once it is complete.
	l3 := line(112, 7064, 1500, 1500.3, 30)
	appendTo(t, p, "\r\n"+l3[:20])
	if u := poll(t, f); len(u.Lines) != 0 {
		t.Fatalf("partial line reported: %+v", u)
	}
	appendTo(t, p, l3[20:]+"\r\ngarbage\r\n")
	if u := poll(t, f); len(u.Lines) != 1 || u.Lines[0].N != 4 || u.Bad != 1 || u.Lines[0].FMeas != 1500.3 {
		t.Fatalf("completed line: %+v", u)
	}

	// Capture renames fmt.all while it is being followed, even mid-read;
	// WSJT-X starts a new one.
	fh, _, _, err := openShared(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p, filepath.Join(dir, "fmt-run1.bak")); err != nil {
		t.Fatalf("rename while open: %v", err)
	}
	fh.Close()
	appendTo(t, p, line(200, 7065, 1500, 1501.0, 30)+"\r\n"+line(206, 7065, 1500, 1501.0, 30)+"\r\n"+line(212, 7065, 1500, 1501.0, 30)+"\r\n"+line(218, 7065, 1500, 1501.0, 30)+"\r\n")
	if u := poll(t, f); !u.Reset || len(u.Lines) != 4 || u.Lines[0].KHz != 7065 || u.Lines[0].N != 1 {
		t.Fatalf("replaced file: %+v", u)
	}

	// The file shrinks (rewritten in place).
	if err := os.WriteFile(p, []byte(line(300, 3598, 1500, 1499.5, 30)+"\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if u := poll(t, f); !u.Reset || len(u.Lines) != 1 || u.Lines[0].KHz != 3598 {
		t.Fatalf("shrunk file: %+v", u)
	}

	// The file disappears, then comes back.
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if u := poll(t, f); !u.Reset || !u.Gone {
		t.Fatalf("deleted file: %+v", u)
	}
	if u := poll(t, f); u.Reset || !u.Gone {
		t.Fatalf("still gone: %+v", u)
	}
	appendTo(t, p, line(400, 3599, 1500, 1500.0, 30)+"\n")
	if u := poll(t, f); u.Reset || u.Gone || len(u.Lines) != 1 {
		t.Fatalf("new file: %+v", u)
	}
}
