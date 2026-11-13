package iq

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"time"
)

// WAVWriter writes stereo 32-bit float samples (left I, right Q) to a WAV
// file. The header starts with the expected rate and centre, is brought up
// to date by Sync while recording (so a crash leaves a readable file), and
// is completed by Close.
//
// The file carries an "auxi" chunk in the layout HDSDR and SDR# use, so those
// programs show the centre frequency and start time.
type WAVWriter struct {
	f       *os.File
	w       *bufio.Writer
	frames  int64
	started time.Time
	buf     []byte
}

const (
	auxiSize   = 16 + 16 + 9*4 // two SYSTEMTIMEs and nine DWORDs
	headerSize = 12 + 8 + 16 + 8 + auxiSize + 8
	maxData    = math.MaxUint32 - headerSize
)

// CreateWAV creates path and writes a header with the expected rate and
// centre frequency and no samples yet.
func CreateWAV(path string, start time.Time, rate int, centerHz int64) (*WAVWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	w := &WAVWriter{f: f, w: bufio.NewWriterSize(f, 1<<20), started: start}
	if _, err := w.w.Write(header(0, rate, centerHz, start, start)); err != nil {
		f.Close()
		return nil, err
	}
	return w, nil
}

// Frames is the number of IQ pairs written so far.
func (w *WAVWriter) Frames() int64 { return w.frames }

// Bytes is the size of the file so far.
func (w *WAVWriter) Bytes() int64 { return headerSize + w.frames*8 }

// Write appends interleaved I,Q samples. A trailing odd sample is dropped.
// Writing stops (with an error) when the file reaches the WAV 4 GB limit.
func (w *WAVWriter) Write(iq []float32) error {
	n := len(iq) &^ 1
	if w.Bytes()+int64(n)*4 > maxData {
		return fmt.Errorf("WAV file is full (4 GB)")
	}
	if cap(w.buf) < n*4 {
		w.buf = make([]byte, n*4)
	}
	b := w.buf[:n*4]
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(iq[i]))
	}
	if _, err := w.w.Write(b); err != nil {
		return err
	}
	w.frames += int64(n / 2)
	return nil
}

// Sync writes out the samples so far and brings the header up to date, so
// the file is readable as it stands.
func (w *WAVWriter) Sync(rate int, centerHz int64, stop time.Time) error {
	if err := w.w.Flush(); err != nil {
		return err
	}
	_, err := w.f.WriteAt(header(w.frames, rate, centerHz, w.started, stop), 0)
	return err
}

// Close completes the header with the final rate and centre frequency.
func (w *WAVWriter) Close(rate int, centerHz int64, stop time.Time) error {
	err := w.Sync(rate, centerHz, stop)
	if e := w.f.Close(); err == nil {
		err = e
	}
	return err
}

func header(frames int64, rate int, centerHz int64, start, stop time.Time) []byte {
	const channels, bits = 2, 32
	data := uint32(frames * 8)
	b := make([]byte, 0, headerSize)
	le := binary.LittleEndian
	b = append(b, "RIFF"...)
	b = le.AppendUint32(b, uint32(headerSize-8)+data)
	b = append(b, "WAVE"...)
	b = append(b, "fmt "...)
	b = le.AppendUint32(b, 16)
	b = le.AppendUint16(b, 3) // IEEE float
	b = le.AppendUint16(b, channels)
	b = le.AppendUint32(b, uint32(rate))
	b = le.AppendUint32(b, uint32(rate*channels*bits/8))
	b = le.AppendUint16(b, channels*bits/8)
	b = le.AppendUint16(b, bits)
	b = append(b, "auxi"...)
	b = le.AppendUint32(b, auxiSize)
	b = appendSystemTime(b, start)
	b = appendSystemTime(b, stop)
	b = le.AppendUint32(b, uint32(centerHz)) // CenterFreq
	b = le.AppendUint32(b, uint32(rate))     // ADFrequency
	b = le.AppendUint32(b, 0)                // IFFrequency
	b = le.AppendUint32(b, uint32(rate))     // Bandwidth
	b = le.AppendUint32(b, 0)                // IQOffset
	for i := 0; i < 4; i++ {
		b = le.AppendUint32(b, 0) // unused
	}
	b = append(b, "data"...)
	b = le.AppendUint32(b, data)
	return b
}

// appendSystemTime writes a Windows SYSTEMTIME in UTC.
func appendSystemTime(b []byte, t time.Time) []byte {
	t = t.UTC()
	le := binary.LittleEndian
	for _, v := range []int{t.Year(), int(t.Month()), int(t.Weekday()), t.Day(),
		t.Hour(), t.Minute(), t.Second(), t.Nanosecond() / 1e6} {
		b = le.AppendUint16(b, uint16(v))
	}
	return b
}
