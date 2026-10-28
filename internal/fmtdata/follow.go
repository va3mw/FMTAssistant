package fmtdata

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
)

// Follower reads lines appended to a growing fmt.all, like "tail -f".
//
// It never writes, locks or deletes the file: each Poll opens it read-only,
// sharing read, write and delete with everyone else (so WSJT-X can keep
// writing and Capture can rename it), reads what is new and closes it again.
// When the file shrinks, is replaced by a new file (Capture or "Start a fresh
// fmt.all" renames it and WSJT-X starts another) or disappears, the follower
// starts over from the beginning and says so with Reset.
type Follower struct {
	Path string

	open    bool   // a file was being followed at the last Poll
	id      fileID // identity of that file
	off     int64  // bytes consumed
	n       int    // lines consumed (blank ones included, as Parse counts them)
	partial []byte // an unfinished last line
}

// Update is what changed since the previous Poll.
type Update struct {
	Reset bool   `json:"reset"` // forget everything read so far
	Gone  bool   `json:"gone"`  // the file does not exist now
	Lines []Line `json:"lines"` // new measurements, in file order
	Bad   int    `json:"bad"`   // new lines that could not be read
}

const maxPartial = 1 << 20

func (f *Follower) restart() {
	f.open, f.off, f.n, f.partial = false, 0, 0, nil
}

// Poll reads any new complete lines.
func (f *Follower) Poll() (Update, error) {
	var u Update
	fh, id, size, err := openShared(f.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			u.Reset, u.Gone = f.open, true
			f.restart()
			return u, nil
		}
		return u, err
	}
	defer fh.Close()
	if f.open && (id != f.id || size < f.off) {
		u.Reset = true
		f.restart()
	}
	f.open, f.id = true, id
	if size == f.off {
		return u, nil
	}
	if _, err := fh.Seek(f.off, io.SeekStart); err != nil {
		return u, err
	}
	buf := make([]byte, size-f.off)
	k, err := io.ReadFull(fh, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return u, err
	}
	buf = buf[:k]
	f.off += int64(k)
	data := append(f.partial, buf...)
	last := bytes.LastIndexByte(data, '\n')
	if last < 0 {
		f.partial = data
		if len(f.partial) > maxPartial {
			f.partial = nil
		}
		return u, nil
	}
	f.partial = append([]byte(nil), data[last+1:]...)
	for _, s := range strings.Split(string(data[:last]), "\n") {
		f.n++
		raw := strings.TrimRight(s, "\x00\r\n\t ")
		if strings.TrimSpace(raw) == "" {
			continue
		}
		l, ok := ParseLine(raw)
		if !ok {
			u.Bad++
			continue
		}
		l.N = f.n
		u.Lines = append(u.Lines, l)
	}
	return u, nil
}
