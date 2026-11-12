// Command iqrec is a prototype that records a FlexRadio DAX IQ stream to a
// WAV file with a JSON description beside it. It is a test bed for adding IQ
// recording to FMT Assistant: it uses the app's own radio client and never
// transmits. The only radio setting it changes is the DAX IQ channel of the
// slice's panadapter, and only if it had none, and it puts that back.
//
//	iqrec -slice A -seconds 60
//	iqrec -ip 192.168.1.50 -station "SHACK" -slice B -rate 48000 -out "C:\FMT\IQ"
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/va3mw/FMTAssistant/internal/flex"
	"github.com/va3mw/FMTAssistant/internal/logx"
)

func main() {
	ip := flag.String("ip", "", "radio IP address (default: find it on the LAN)")
	station := flag.String("station", "", "SmartSDR station name (default: the only one)")
	slice := flag.String("slice", "A", "slice letter whose panadapter to record")
	seconds := flag.Int("seconds", 60, "how long to record; Ctrl+C stops early")
	rate := flag.Int("rate", 24000, "DAX IQ sample rate: 24000, 48000, 96000 or 192000")
	out := flag.String("out", ".", "folder for the recording")
	name := flag.String("name", "", "file name without extension (default: IQ <UTC time> <slice kHz>)")
	swap := flag.Bool("swap", false, "swap I and Q (if a carrier above the centre shows up below it)")
	note := flag.String("note", "", "free text to keep in the JSON file")
	verbose := flag.Bool("v", false, "show every radio API line")
	flag.Parse()

	if err := os.MkdirAll(*out, 0o755); err != nil {
		fatal("cannot create %s: %v", *out, err)
	}
	logs := logx.New(filepath.Join(*out, "iqrec-logs"))
	defer logs.Close()
	logs.SetTraffic(*verbose)
	logs.OnEntry(func(e logx.Entry) {
		if *verbose || (e.Level != "TRAFFIC" && e.Level != "DEBUG") {
			fmt.Printf("%s %-5s %-9s %s\n", e.Time, e.Level, e.Source, e.Msg)
		}
	})

	model := ""
	if *ip == "" {
		*ip, model = discover(logs)
	}
	radio := flex.NewRadio(logs, nil)
	radio.Connect(*ip, model, *station)
	defer radio.Disconnect()

	sl, pan := waitReady(radio, *slice)
	fmt.Printf("\nSlice %s: %.3f kHz %s on panadapter 0x%08X, centre %.3f kHz, %.1f kHz wide, DAX IQ channel %d\n",
		sl.Letter, float64(sl.FreqHz)/1000, sl.Mode, pan.ID, pan.CenterHz/1000, pan.BandwidthHz/1000, pan.DAXIQChannel)

	if *name == "" {
		*name = fmt.Sprintf("IQ %s %.3f kHz", time.Now().UTC().Format("2006-01-02 1504Z"), float64(sl.FreqHz)/1000)
	}
	path := filepath.Join(*out, *name)
	rec, err := radio.StartIQ(flex.IQOptions{Slice: *slice, Rate: *rate, Path: path, SwapIQ: *swap, Note: *note})
	if err != nil {
		fatal("%v", err)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	end := time.After(time.Duration(*seconds) * time.Second)
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	start := time.Now()
	fmt.Printf("Recording for %d s to %s.wav (Ctrl+C to stop)\n", *seconds, path)
loop:
	for {
		select {
		case <-stop:
			fmt.Println("\nStopping.")
			break loop
		case <-end:
			break loop
		case <-tick.C:
			st := rec.Status()
			if st.Err != nil {
				break loop
			}
			if st.Packets == 0 {
				fmt.Printf("%3.0f s  no IQ packets yet. Is Windows Firewall blocking iqrec? Allow it on private networks.\n", time.Since(start).Seconds())
				continue
			}
			fmt.Printf("%3.0f s  %d packets, %.1f s of IQ at %d/s, %d gaps\n",
				time.Since(start).Seconds(), st.Packets, float64(st.Samples)/float64(st.SampleRate), st.SampleRate, st.Gaps)
		}
	}

	meta, err := rec.Stop()
	fmt.Println()
	if meta.Samples > 0 {
		fmt.Printf("Wrote %s.wav (%.1f s, %d samples/s, I left, Q right) and %s.json\n",
			path, float64(meta.Samples)/float64(meta.SampleRate), meta.SampleRate, path)
		fmt.Printf("IQ centre %.3f Hz; %d packets, %d gaps (%d samples filled with silence)\n",
			meta.CenterHz, meta.Packets, len(meta.Gaps), meta.ZeroFilled)
		if meta.FirstPacket != nil {
			fmt.Printf("VITA timestamps: TSI %d, TSF %d\n", meta.FirstPacket.TSI, meta.FirstPacket.TSF)
		}
	}
	if meta.Peak != nil {
		fmt.Printf("Strongest signal: %.1f Hz (%+.1f Hz from the IQ centre, %+.1f Hz from the slice dial), %.0f dB over the noise\n",
			meta.PeakHz, meta.Peak.OffsetHz, meta.PeakHz-float64(meta.SliceStart.FreqHz), meta.Peak.SNRdB)
		fmt.Println("  Check this against where the carrier really is (for example dial + WSJT-X's fMeas).")
		fmt.Println("  If it shows on the wrong side of the centre, run again with -swap.")
	}
	for _, p := range meta.Problems {
		fmt.Println("Problem:", p)
	}
	if err != nil {
		fatal("%v", err)
	}
}

// discover waits for one radio's discovery broadcast.
func discover(l *logx.Logger) (ip, model string) {
	d := flex.NewDiscovery(l, nil)
	d.Start()
	defer d.Stop()
	fmt.Println("Looking for a FlexRadio on the LAN...")
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		if list := d.List(); len(list) == 1 {
			return list[0].IP, list[0].Model
		}
	}
	list := d.List()
	if len(list) == 0 {
		fatal("no radio found; give its address with -ip")
	}
	var b strings.Builder
	for _, r := range list {
		fmt.Fprintf(&b, "\n  %s %q at %s", r.Model, r.Nickname, r.IP)
	}
	fatal("more than one radio found; choose one with -ip:%s", b.String())
	return "", ""
}

// waitReady waits until we're bound to a station and know the slice and its pan.
func waitReady(r *flex.Radio, letter string) (flex.Slice, flex.Pan) {
	deadline := time.Now().Add(15 * time.Second)
	for {
		s := r.Snapshot()
		if sl, ok := s.SliceByLetter(letter); ok && sl.Pan != 0 {
			if pan, ok := s.PanByID(sl.Pan); ok {
				return sl, pan
			}
		}
		if time.Now().After(deadline) {
			switch {
			case !s.Connected:
				fatal("could not connect to the radio")
			case s.BoundHandle == 0:
				var names []string
				for _, st := range s.Stations {
					names = append(names, fmt.Sprintf("%q", st.Name))
				}
				if len(names) == 0 {
					fatal("no SmartSDR station is connected to the radio")
				}
				fatal("choose a station with -station: %s", strings.Join(names, ", "))
			default:
				if sl, ok := s.SliceByLetter(letter); ok {
					fatal("slice %s is open but its panadapter (0x%08X) hasn't reported its status", letter, sl.Pan)
				}
				fatal("slice %s is not open on station %q", letter, s.BoundStation)
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "iqrec: "+f+"\n", a...)
	os.Exit(1)
}
