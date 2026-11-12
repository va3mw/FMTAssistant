package flex

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"math"
	"os"
)

// IQWav writes complex samples as a 2-channel 32-bit float WAV file: I on the
// left channel, Q on the right. Audacity, HDSDR, SDR#, GNU Radio and most
// SDR tools open it. The header sizes are patched every few seconds and on
// Close, so a file cut short by a crash is still readable.
type IQWav struct {
	f        *os.File
	w        *bufio.Writer
	rate     int
	frames   int64
	sinceFix int64
	buf      [8]byte
}

const wavHeaderSize = 58 // RIFF(12) + fmt(8+18) + fact(8+4) + data(8)

// CreateIQWav creates path, replacing any file there.
func CreateIQWav(path string, rate int) (*IQWav, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	w := &IQWav{f: f, w: bufio.NewWriterSize(f, 256*1024), rate: rate}
	if _, err := w.w.Write(w.header()); err != nil {
		f.Close()
		return nil, err
	}
	return w, nil
}

// header builds the WAV header for the frames written so far.
func (w *IQWav) header() []byte {
	const channels, bits = 2, 32
	dataBytes := uint32(w.frames * channels * bits / 8)
	h := make([]byte, 0, wavHeaderSize)
	le32 := func(v uint32) { h = binary.LittleEndian.AppendUint32(h, v) }
	le16 := func(v uint16) { h = binary.LittleEndian.AppendUint16(h, v) }
	h = append(h, "RIFF"...)
	le32(wavHeaderSize - 8 + dataBytes)
	h = append(h, "WAVE"...)
	h = append(h, "fmt "...)
	le32(18)
	le16(3) // WAVE_FORMAT_IEEE_FLOAT
	le16(channels)
	le32(uint32(w.rate))
	le32(uint32(w.rate * channels * bits / 8))
	le16(channels * bits / 8)
	le16(bits)
	le16(0) // no extension
	h = append(h, "fact"...)
	le32(4)
	le32(uint32(w.frames))
	h = append(h, "data"...)
	le32(dataBytes)
	return h
}

// Write appends samples.
func (w *IQWav) Write(s []complex64) error {
	for _, v := range s {
		binary.LittleEndian.PutUint32(w.buf[0:], math.Float32bits(real(v)))
		binary.LittleEndian.PutUint32(w.buf[4:], math.Float32bits(imag(v)))
		if _, err := w.w.Write(w.buf[:]); err != nil {
			return err
		}
	}
	return w.advance(int64(len(s)))
}

// WriteZeros appends n silent samples, to hold the place of lost packets so
// the file's timing stays right.
func (w *IQWav) WriteZeros(n int) error {
	var zero [8]byte
	for i := 0; i < n; i++ {
		if _, err := w.w.Write(zero[:]); err != nil {
			return err
		}
	}
	return w.advance(int64(n))
}

func (w *IQWav) advance(n int64) error {
	w.frames += n
	w.sinceFix += n
	if w.frames*8 > math.MaxUint32-wavHeaderSize {
		return fmt.Errorf("WAV file full (4 GB)")
	}
	if w.sinceFix >= int64(w.rate)*5 {
		w.sinceFix = 0
		return w.fixHeader()
	}
	return nil
}

// fixHeader writes the current sizes into the header.
func (w *IQWav) fixHeader() error {
	if err := w.w.Flush(); err != nil {
		return err
	}
	if _, err := w.f.WriteAt(w.header(), 0); err != nil {
		return err
	}
	return nil
}

// Frames is the number of samples written.
func (w *IQWav) Frames() int64 { return w.frames }

// Close finishes the header and closes the file.
func (w *IQWav) Close() error {
	err := w.fixHeader()
	if cerr := w.f.Close(); err == nil {
		err = cerr
	}
	return err
}
