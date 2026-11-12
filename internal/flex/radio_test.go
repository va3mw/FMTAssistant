package flex

import (
	"testing"

	"github.com/va3mw/FMTAssistant/internal/logx"
)

func TestReferenceStatus(t *testing.T) {
	l := logx.New(t.TempDir())
	defer l.Close()
	r := NewRadio(l, nil)
	var got []Reference
	r.OnReference = func(ref Reference) { got = append(got, ref) }

	if ref := r.Snapshot().Ref; ref.Known {
		t.Fatalf("known before any status: %+v", ref)
	}
	r.handleLine("S12345678|radio oscillator state=gpsdo setting=auto locked=1 ext_present=0 gpsdo_present=1 tcxo_present=1 gnss_present=0")
	ref := r.Snapshot().Ref
	if !ref.Known || ref.State != "gpsdo" || ref.Setting != "auto" || !ref.Locked || !ref.GPSDO || !ref.TCXO || ref.External || ref.GNSS {
		t.Fatalf("oscillator: %+v", ref)
	}
	r.handleLine("S12345678|gps lat=43.1#lon=-79.9#grid=FN03#freq_error=0.12#status=Fine\x7fLock#fix=TIME_ONLY#time=02:34:07")
	ref = r.Snapshot().Ref
	if ref.GPSFreqError != "0.12" || ref.GPSStatus != "Fine Lock" || ref.GPSFix != "TIME_ONLY" || ref.GPSTime != "02:34:07" || ref.GPSTimeAt.IsZero() {
		t.Fatalf("gps: %+v", ref)
	}
	// The same oscillator state again is not a change.
	r.handleLine("S12345678|radio oscillator state=gpsdo setting=auto locked=1")
	// Auto falls back to the TCXO when the GPSDO is lost.
	r.handleLine("S12345678|radio oscillator state=tcxo setting=auto locked=0")
	if len(got) != 2 || got[0].State != "gpsdo" || got[1].State != "tcxo" || got[1].Locked {
		t.Fatalf("callbacks: %+v", got)
	}
	// An ordinary radio status line still works and doesn't touch the reference.
	r.handleLine("S12345678|radio nickname=Shack\x7fRadio callsign=VA3MW")
	if s := r.Snapshot(); s.Nickname != "Shack Radio" || s.Ref.State != "tcxo" {
		t.Fatalf("radio status: %q %+v", s.Nickname, s.Ref)
	}
}

func TestParseSliceError(t *testing.T) {
	if v, err := ParseSliceError("+3.814698,-9.536743"); err != nil || v != 3.814698 {
		t.Fatalf("got %v %v", v, err)
	}
	if v, err := ParseSliceError(" -1.5 "); err != nil || v != -1.5 {
		t.Fatalf("rx only: %v %v", v, err)
	}
	if _, err := ParseSliceError("garbage"); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestPanAndSlicePan(t *testing.T) {
	l := logx.New(t.TempDir())
	defer l.Close()
	r := NewRadio(l, nil)
	r.pans, r.slices = map[string]*Pan{}, map[int]*Slice{}
	r.handleLine("S12345678|display pan 0x40000000 center=7.064000 bandwidth=0.200000 daxiq_channel=1 client_handle=0x1234")
	r.handleLine("S12345678|slice 0 in_use=1 RF_frequency=7.062500 mode=DIGU index_letter=A pan=0x40000000 client_handle=0x1234")
	p, ok := r.Pan("0x40000000")
	if !ok || p.CenterHz != 7064000 || p.BandwidthHz != 200000 || p.DAXIQChannel != 1 || p.ClientHandle != 0x1234 {
		t.Fatalf("pan %+v %v", p, ok)
	}
	if s := r.Snapshot(); len(s.Slices) != 1 || s.Slices[0].Pan != "0x40000000" || len(s.Pans) != 1 {
		t.Fatalf("snapshot %+v", s)
	}
	r.handleLine("S12345678|display pan 0x40000000 removed")
	if _, ok := r.Pan("0x40000000"); ok {
		t.Fatal("pan not removed")
	}
}

func TestParseStreamID(t *testing.T) {
	for in, want := range map[string]uint32{"20000000": 0x20000000, "0x20000001": 0x20000001, "stream_id=0x2000000A": 0x2000000A} {
		if got, err := ParseStreamID(in); err != nil || got != want {
			t.Errorf("ParseStreamID(%q)=%x %v", in, got, err)
		}
	}
	if _, err := ParseStreamID(""); err == nil {
		t.Error("empty reply accepted")
	}
}
