package flex

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/va3mw/FMTAssistant/internal/logx"
)

// vitaIQ builds a DAX IQ packet the way the radio sends it.
func vitaIQ(stream uint32, count uint8, class uint16, ts uint32, frac uint64, s []complex64) []byte {
	words := 1 + 1 + 2 + 1 + 2 + 2*len(s)
	b := make([]byte, 0, words*4)
	hdr := uint32(1)<<28 | 1<<27 | 1<<22 | 1<<20 | uint32(count&0xF)<<16 | uint32(words)
	b = binary.BigEndian.AppendUint32(b, hdr)
	b = binary.BigEndian.AppendUint32(b, stream)
	b = binary.BigEndian.AppendUint32(b, flexOUI)
	b = binary.BigEndian.AppendUint32(b, 0x534C<<16|uint32(class))
	b = binary.BigEndian.AppendUint32(b, ts)
	b = binary.BigEndian.AppendUint64(b, frac)
	for _, v := range s {
		b = binary.BigEndian.AppendUint32(b, math.Float32bits(real(v)))
		b = binary.BigEndian.AppendUint32(b, math.Float32bits(imag(v)))
	}
	return b
}

func TestParseVitaIQ(t *testing.T) {
	in := []complex64{complex(0.25, -0.5), complex(1, 2)}
	p, err := ParseVita(vitaIQ(0x20000001, 7, 0x02E4, 1700000000, 1234, in))
	if err != nil {
		t.Fatal(err)
	}
	if p.Type != 1 || p.StreamID != 0x20000001 || p.Count != 7 || p.OUI != flexOUI || p.PacketClass != 0x02E4 ||
		p.TSI != 1 || p.TSF != 1 || p.IntTS != 1700000000 || p.FracTS != 1234 {
		t.Fatalf("header: %+v", p)
	}
	if DAXIQRate(p.PacketClass) != 48000 {
		t.Fatalf("rate %d", DAXIQRate(p.PacketClass))
	}
	got := IQSamples(p.Payload, false, nil)
	if len(got) != 2 || got[0] != in[0] || got[1] != in[1] {
		t.Fatalf("samples %v", got)
	}
	if sw := IQSamples(p.Payload, true, nil); sw[0] != complex(-0.5, 0.25) {
		t.Fatalf("swapped %v", sw)
	}
	// Truncated and lying packets are refused, not read past the end.
	b := vitaIQ(1, 0, 0x02E3, 0, 0, in)
	if _, err := ParseVita(b[:12]); err == nil {
		t.Fatal("truncated packet accepted")
	}
	if _, err := ParseVita([]byte{0x18, 0, 0, 1}); err == nil {
		t.Fatal("header-only packet with stream id accepted")
	}
}

func TestIQWav(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.wav")
	w, err := CreateIQWav(path, 24000)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write([]complex64{complex(0.5, -0.25)}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteZeros(2); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if len(b) != wavHeaderSize+3*8 {
		t.Fatalf("size %d", len(b))
	}
	le := binary.LittleEndian
	if string(b[0:4]) != "RIFF" || le.Uint32(b[4:]) != uint32(len(b)-8) || string(b[8:16]) != "WAVEfmt " {
		t.Fatalf("riff header % x", b[:16])
	}
	if le.Uint16(b[20:]) != 3 || le.Uint16(b[22:]) != 2 || le.Uint32(b[24:]) != 24000 || le.Uint32(b[28:]) != 192000 || le.Uint16(b[34:]) != 32 {
		t.Fatalf("fmt chunk % x", b[12:38])
	}
	if string(b[38:42]) != "fact" || le.Uint32(b[46:]) != 3 || string(b[50:54]) != "data" || le.Uint32(b[54:]) != 24 {
		t.Fatalf("fact/data % x", b[38:58])
	}
	if math.Float32frombits(le.Uint32(b[58:])) != 0.5 || math.Float32frombits(le.Uint32(b[62:])) != -0.25 {
		t.Fatal("first sample")
	}
}

func tone(rate int, hz float64, from, n int) []complex64 {
	s := make([]complex64, n)
	for i := range s {
		ph := 2 * math.Pi * hz * float64(from+i) / float64(rate)
		s[i] = complex(float32(0.1*math.Cos(ph)), float32(0.1*math.Sin(ph)))
	}
	return s
}

func TestSpectrumPeak(t *testing.T) {
	for _, hz := range []float64{1500.3, -2210.7} {
		s := NewSpectrum(24000)
		s.Add(tone(24000, hz, 0, 24000*5))
		pk, ok := s.Peak()
		if !ok || math.Abs(pk.OffsetHz-hz) > 0.3 || pk.SNRdB < 60 {
			t.Fatalf("%v Hz: %+v", hz, pk)
		}
	}
}

func TestPanAndStreamStatus(t *testing.T) {
	l := logx.New(t.TempDir())
	defer l.Close()
	r := NewRadio(l, nil)
	r.pans, r.streams, r.slices = map[uint32]*Pan{}, map[uint32]*Stream{}, map[int]*Slice{}
	r.handleLine("S1|display pan 0x40000000 center=7.061500 bandwidth=0.200000 daxiq_channel=0 client_handle=0x1234")
	r.handleLine("S1|slice 0 in_use=1 RF_frequency=7.061000 mode=DIGU pan=0x40000000 index_letter=A")
	r.handleLine("S1|stream 0x20000001 type=dax_iq daxiq_channel=2 pan=0x40000000 daxiq_rate=48000 client_handle=0x99")
	s := r.Snapshot()
	p, ok := s.PanByID(0x40000000)
	if !ok || p.CenterHz != 7061500 || p.BandwidthHz != 200000 || p.ClientHandle != 0x1234 || s.Slices[0].Pan != 0x40000000 {
		t.Fatalf("pan %+v slices %+v", p, s.Slices)
	}
	if st, ok := r.stream(0x20000001); !ok || st.Type != "dax_iq" || st.DAXIQChannel != 2 || st.Rate != 48000 || st.Pan != 0x40000000 {
		t.Fatalf("stream %+v", st)
	}
	r.handleLine("S1|stream 0x20000001 removed")
	r.handleLine("S1|display pan 0x40000000 removed")
	if _, ok := r.stream(0x20000001); ok {
		t.Fatal("stream not removed")
	}
	if s := r.Snapshot(); len(s.Pans) != 0 {
		t.Fatal("pan not removed")
	}
}

// fakeRadio answers API commands on one end of a pipe, like a radio would,
// and sends DAX IQ packets to the UDP port it is told.
type fakeRadio struct {
	t    *testing.T
	r    *Radio
	mu   sync.Mutex
	cmds []string
	sent chan struct{}
}

func (f *fakeRadio) serve(conn net.Conn) {
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		seq, cmd, _ := strings.Cut(strings.TrimPrefix(sc.Text(), "C"), "|")
		f.mu.Lock()
		f.cmds = append(f.cmds, cmd)
		f.mu.Unlock()
		reply := ""
		switch {
		case strings.HasPrefix(cmd, "client udpport "):
			var port int
			fmt.Sscanf(cmd, "client udpport %d", &port)
			go f.send(port)
		case strings.HasPrefix(cmd, "stream create"):
			reply = "20000001"
		case strings.HasPrefix(cmd, "slice get_error"):
			reply = "+1.5,0"
		case strings.HasPrefix(cmd, "display pan set"):
			ch := cmd[strings.LastIndex(cmd, "=")+1:]
			f.r.handleLine("S1|display pan 0x40000000 daxiq_channel=" + ch)
		}
		f.r.handleLine("R" + seq + "|0|" + reply)
	}
}

// send waits for the stream to exist, then sends 200 packets of a tone 1500 Hz
// above the centre, leaving out packet 10, plus one packet of another stream.
func (f *fakeRadio) send(port int) {
	defer close(f.sent)
	c, err := net.Dial("udp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		f.t.Error(err)
		return
	}
	defer c.Close()
	time.Sleep(100 * time.Millisecond)
	const per = 128
	c.Write(vitaIQ(0x20000099, 0, 0x02E3, 0, 0, tone(24000, 500, 0, per)))
	for i := 0; i < 200; i++ {
		if i == 10 {
			continue
		}
		c.Write(vitaIQ(0x20000001, uint8(i), 0x02E3, 1700000000, uint64(i*per), tone(24000, 1500, i*per, per)))
		time.Sleep(200 * time.Microsecond)
	}
	time.Sleep(100 * time.Millisecond)
}

func TestIQRecordingEndToEnd(t *testing.T) {
	dir := t.TempDir()
	l := logx.New(dir)
	defer l.Close()
	r := NewRadio(l, nil)
	ours, theirs := net.Pipe()
	defer ours.Close()
	r.conn, r.connected, r.pending = ours, true, map[uint32]replyFn{}
	r.slices, r.pans, r.streams, r.stations = map[int]*Slice{}, map[uint32]*Pan{}, map[uint32]*Stream{}, map[uint32]*Station{}
	r.boundHandle, r.boundName = 0x1234, "SHACK"
	r.handleLine("S1|display pan 0x40000000 center=7.059500 bandwidth=0.200000 daxiq_channel=0 client_handle=0x1234")
	r.handleLine("S1|slice 0 in_use=1 RF_frequency=7.061000 mode=DIGU pan=0x40000000 index_letter=A client_handle=0x1234")
	f := &fakeRadio{t: t, r: r, sent: make(chan struct{})}
	go f.serve(theirs)

	path := filepath.Join(dir, "Run 1 IQ")
	rec, err := r.StartIQ(IQOptions{Slice: "A", Rate: 24000, Path: path, Note: "test"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.sent:
	case <-time.After(5 * time.Second):
		t.Fatal("fake radio never sent")
	}
	meta, err := rec.Stop()
	if err != nil {
		t.Fatal(err)
	}

	f.mu.Lock()
	cmds := strings.Join(f.cmds, "\n")
	f.mu.Unlock()
	for _, want := range []string{
		"display pan set 0x40000000 daxiq_channel=1",
		"stream create type=dax_iq daxiq_channel=1",
		"stream set 0x20000001 daxiq_rate=24000",
		"stream remove 0x20000001",
		"display pan set 0x40000000 daxiq_channel=0",
	} {
		if !strings.Contains(cmds, want) {
			t.Errorf("missing command %q in:\n%s", want, cmds)
		}
	}
	if meta.Packets != 199 || meta.Samples != 200*128 || len(meta.Gaps) != 1 || meta.Gaps[0].MissingPackets != 1 ||
		meta.Gaps[0].AtSample != 10*128 || meta.ZeroFilled != 128 || meta.OtherPackets != 1 {
		t.Fatalf("counts: packets %d samples %d gaps %+v zero %d other %d", meta.Packets, meta.Samples, meta.Gaps, meta.ZeroFilled, meta.OtherPackets)
	}
	if meta.SampleRate != 24000 || meta.CenterHz != 7059500 || !meta.ChannelSet || meta.DAXIQChannel != 1 ||
		meta.SliceErrStart == nil || *meta.SliceErrStart != 1.5 || meta.FirstPacket == nil || meta.FirstPacket.Int != 1700000000 {
		t.Fatalf("meta: %+v", meta)
	}
	if meta.Peak == nil || math.Abs(meta.PeakHz-7061000) > 2 {
		t.Fatalf("peak %+v at %.1f Hz", meta.Peak, meta.PeakHz)
	}
	var onDisk IQMeta
	b, err := os.ReadFile(path + ".json")
	if err != nil || json.Unmarshal(b, &onDisk) != nil || onDisk.Wav != "Run 1 IQ.wav" || onDisk.Note != "test" {
		t.Fatalf("json %v: %s", err, b)
	}
	if st, err := os.Stat(path + ".wav"); err != nil || st.Size() != wavHeaderSize+200*128*8 {
		t.Fatalf("wav %v %v", st, err)
	}
	if _, err := rec.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}
