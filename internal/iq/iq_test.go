package iq

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// vita builds an IF data packet with stream ID, class ID and timestamps.
func vita(count uint8, stream uint32, samples []float32, order binary.ByteOrder) []byte {
	words := 1 + 1 + 2 + 1 + 2 + len(samples)
	b := make([]byte, words*4)
	be := binary.BigEndian
	w0 := uint32(1)<<28 | 1<<27 | 1<<22 | 1<<20 | uint32(count&0xF)<<16 | uint32(words)
	be.PutUint32(b[0:], w0)
	be.PutUint32(b[4:], stream)
	be.PutUint32(b[8:], 0x001C2D)
	be.PutUint32(b[12:], 0x534C<<16|0x02E3)
	be.PutUint32(b[16:], 1700000000)
	be.PutUint64(b[20:], 1234)
	for i, s := range samples {
		order.PutUint32(b[28+i*4:], math.Float32bits(s))
	}
	return b
}

func TestParseVITA(t *testing.T) {
	b := vita(5, 0x20000001, []float32{0.5, -0.25, 0.125, 0}, binary.BigEndian)
	p, err := ParseVITA(b)
	if err != nil {
		t.Fatal(err)
	}
	if p.Type != 1 || p.Count != 5 || p.StreamID != 0x20000001 || !p.HasClass || p.OUI != 0x1C2D ||
		p.InfoClass != 0x534C || p.PacketClass != 0x02E3 || p.IntTS != 1700000000 || p.FracTS != 1234 || len(p.Payload) != 16 {
		t.Fatalf("parsed %+v", p)
	}
	if _, err := ParseVITA(b[:10]); err == nil {
		t.Fatal("truncated packet accepted")
	}
}

func TestDetectOrder(t *testing.T) {
	s := []float32{0.01, -0.3, 0.7, -0.002, 1e-4, 0.5}
	for _, c := range []struct {
		order binary.ByteOrder
		want  Order
	}{{binary.BigEndian, OrderBig}, {binary.LittleEndian, OrderLittle}} {
		p, _ := ParseVITA(vita(0, 1, s, c.order))
		if got := DetectOrder(p.Payload); got != c.want {
			t.Errorf("%v: detected %v", c.order, got)
		}
		d := Decode(nil, p.Payload, c.want)
		for i := range s {
			if d[i] != s[i] {
				t.Fatalf("decoded %v, want %v", d, s)
			}
		}
	}
	if DetectOrder(make([]byte, 16)) != OrderUnknown {
		t.Error("zeros should be unknown")
	}
}

func TestCountGap(t *testing.T) {
	for _, c := range []struct {
		prev, cur uint8
		want      int
	}{{3, 4, 0}, {15, 0, 0}, {3, 6, 2}, {14, 1, 2}, {5, 5, 15}} {
		if g := CountGap(c.prev, c.cur); g != c.want {
			t.Errorf("CountGap(%d,%d)=%d want %d", c.prev, c.cur, g, c.want)
		}
	}
}

func TestRecorder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run1.wav")
	r, err := NewRecorder(path, Meta{Step: "run1", CenterHz: 7062500, DialHz: 7062500, CarrierHz: 7064000, RequestedRate: 24000})
	if err != nil {
		t.Fatal(err)
	}
	samples := make([]float32, 256) // 128 IQ pairs
	for i := range samples {
		samples[i] = float32(math.Sin(float64(i) / 10))
	}
	t0 := time.Date(2026, 11, 13, 2, 34, 0, 0, time.UTC)
	cnt := uint8(0)
	for i := 0; i < 10; i++ {
		if i == 5 {
			cnt += 2 // two packets lost
		}
		p, _ := ParseVITA(vita(cnt, 7, samples, binary.BigEndian))
		r.Packet(p, t0.Add(time.Duration(i)*time.Millisecond))
		cnt++
	}
	r.Event(Event{What: "center", Hz: 7063000})
	r.SetRate(24000)
	m, err := r.Close()
	if err != nil {
		t.Fatal(err)
	}
	if m.Frames != 1280 || m.Packets != 10 || m.Lost != 2 || m.LostFrames != 256 || m.SampleRate != 24000 ||
		m.ByteOrder != "big-endian" || m.CarrierOffset != 1500 || len(m.Events) != 1 || m.Events[0].Frame != 1280 {
		t.Fatalf("meta %+v", m)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	le := binary.LittleEndian
	if string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" || le.Uint16(b[20:]) != 3 || le.Uint16(b[22:]) != 2 ||
		le.Uint32(b[24:]) != 24000 || string(b[36:40]) != "auxi" {
		t.Fatalf("bad header % x", b[:48])
	}
	if c := le.Uint32(b[44+32:]); c != 7062500 {
		t.Fatalf("auxi centre %d", c)
	}
	if int(le.Uint32(b[4:]))+8 != len(b) {
		t.Fatalf("RIFF size %d, file %d", le.Uint32(b[4:]), len(b))
	}
	if string(b[headerSize-8:headerSize-4]) != "data" || int(le.Uint32(b[headerSize-4:])) != 1280*8 {
		t.Fatal("bad data chunk")
	}
	if got := math.Float32frombits(le.Uint32(b[headerSize+4:])); got != samples[1] {
		t.Fatalf("first Q %v want %v", got, samples[1])
	}
	var side Meta
	jb, err := os.ReadFile(r.MetaPath())
	if err != nil || json.Unmarshal(jb, &side) != nil || side.WAV != "run1.wav" || side.FirstVITA == nil || side.FirstVITA.Integer != 1700000000 {
		t.Fatalf("sidecar %v %s", err, jb)
	}
}

func TestSnapRate(t *testing.T) {
	for m, want := range map[float64]int{23500: 24000, 47000: 48000, 99000: 96000, 180000: 192000} {
		if g := SnapRate(m); g != want {
			t.Errorf("SnapRate(%v)=%d", m, g)
		}
	}
}
