package iq

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"
)

// WAVReader reads a 2-channel IQ WAV: 32-bit float (as recorded by this
// app), or 16-bit PCM (as written by most SDR programs).
type WAVReader struct {
	Rate     int
	Frames   int64
	Float    bool
	CenterHz int64     // from the auxi chunk, 0 if none
	Start    time.Time // from the auxi chunk, zero if none

	f    *os.File
	r    *bufio.Reader
	left int64 // frames not yet read
	buf  []byte
}

// OpenWAV opens an IQ WAV file and reads its header.
func OpenWAV(path string) (*WAVReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	w := &WAVReader{f: f}
	if err := w.header(); err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", baseName(path), err)
	}
	return w, nil
}

func (w *WAVReader) header() error {
	var riff [12]byte
	if _, err := io.ReadFull(w.f, riff[:]); err != nil {
		return fmt.Errorf("not a WAV file")
	}
	if string(riff[0:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		return fmt.Errorf("not a WAV file")
	}
	le := binary.LittleEndian
	var bits, channels, format uint16
	gotFmt := false
	for {
		var ch [8]byte
		if _, err := io.ReadFull(w.f, ch[:]); err != nil {
			return fmt.Errorf("no data chunk")
		}
		id, size := string(ch[0:4]), int64(le.Uint32(ch[4:]))
		switch id {
		case "fmt ", "auxi":
			b := make([]byte, size)
			if _, err := io.ReadFull(w.f, b); err != nil {
				return fmt.Errorf("truncated %q chunk", id)
			}
			if id == "fmt " && len(b) >= 16 {
				format, channels = le.Uint16(b[0:]), le.Uint16(b[2:])
				w.Rate, bits = int(le.Uint32(b[4:])), le.Uint16(b[14:])
				if format == 0xFFFE && len(b) >= 26 { // WAVE_FORMAT_EXTENSIBLE: subformat GUID
					format = le.Uint16(b[24:])
				}
				gotFmt = true
			}
			if id == "auxi" && len(b) >= 36 {
				w.Start = readSystemTime(b[0:16])
				w.CenterHz = int64(le.Uint32(b[32:]))
			}
		case "data":
			if !gotFmt {
				return fmt.Errorf("data before fmt chunk")
			}
			if channels != 2 {
				return fmt.Errorf("%d channels; an IQ file has 2", channels)
			}
			switch {
			case format == 3 && bits == 32:
				w.Float = true
			case format == 1 && bits == 16:
			default:
				return fmt.Errorf("unsupported sample format (format %d, %d bits)", format, bits)
			}
			if w.Rate <= 0 {
				return fmt.Errorf("no sample rate")
			}
			frameBytes := int64(bits / 8 * 2)
			w.Frames = size / frameBytes
			// A file still being written has a zero size; use the file length.
			if st, err := w.f.Stat(); err == nil {
				pos, _ := w.f.Seek(0, io.SeekCurrent)
				if avail := (st.Size() - pos) / frameBytes; size == 0 || avail < w.Frames {
					w.Frames = avail
				}
			}
			w.left = w.Frames
			w.r = bufio.NewReaderSize(w.f, 1<<20)
			return nil
		default:
			if _, err := w.f.Seek(size+size&1, io.SeekCurrent); err != nil {
				return err
			}
		}
		if size&1 == 1 {
			w.f.Seek(1, io.SeekCurrent)
		}
	}
}

func readSystemTime(b []byte) time.Time {
	le := binary.LittleEndian
	v := func(i int) int { return int(le.Uint16(b[i*2:])) }
	if v(0) < 1990 {
		return time.Time{}
	}
	return time.Date(v(0), time.Month(v(1)), v(3), v(4), v(5), v(6), v(7)*1e6, time.UTC)
}

// Read fills dst with up to len(dst) frames and returns how many it read
// (0 and io.EOF at the end).
func (w *WAVReader) Read(dst []complex64) (int, error) {
	n := len(dst)
	if int64(n) > w.left {
		n = int(w.left)
	}
	if n == 0 {
		return 0, io.EOF
	}
	fb := 4
	if w.Float {
		fb = 8
	}
	if cap(w.buf) < n*fb {
		w.buf = make([]byte, n*fb)
	}
	b := w.buf[:n*fb]
	if _, err := io.ReadFull(w.r, b); err != nil {
		return 0, err
	}
	le := binary.LittleEndian
	for i := 0; i < n; i++ {
		if w.Float {
			dst[i] = complex(math.Float32frombits(le.Uint32(b[i*8:])), math.Float32frombits(le.Uint32(b[i*8+4:])))
		} else {
			dst[i] = complex(float32(int16(le.Uint16(b[i*4:])))/32768, float32(int16(le.Uint16(b[i*4+2:])))/32768)
		}
	}
	w.left -= int64(n)
	return n, nil
}

// Skip moves past n frames without reading them.
func (w *WAVReader) Skip(n int64) error {
	if n > w.left {
		n = w.left
	}
	if n <= 0 {
		return nil
	}
	fb := int64(4)
	if w.Float {
		fb = 8
	}
	bytes := n * fb
	// Use up what the buffer already holds, then seek past the rest.
	b := min(bytes, int64(w.r.Buffered()))
	if _, err := w.r.Discard(int(b)); err != nil {
		return err
	}
	if rest := bytes - b; rest > 0 {
		if _, err := w.f.Seek(rest, io.SeekCurrent); err != nil {
			return err
		}
		w.r.Reset(w.f)
	}
	w.left -= n
	return nil
}

func (w *WAVReader) Close() error { return w.f.Close() }

// LoadMeta reads the JSON written beside a recording. ok is false when there
// is none (a file from another program).
func LoadMeta(wavPath string) (m Meta, ok bool, err error) {
	b, err := os.ReadFile(strings.TrimSuffix(wavPath, ".wav") + ".json")
	if err != nil {
		if os.IsNotExist(err) {
			return m, false, nil
		}
		return m, false, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, false, fmt.Errorf("%s: %w", baseName(wavPath), err)
	}
	return m, true, nil
}
