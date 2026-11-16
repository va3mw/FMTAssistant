// Command iqfixture measures the four FMT 2026-11-13 key-down minutes from
// the night's IQ recording(s) and writes the 4-second segments as a small
// JSON test fixture, so the recordings themselves never go in the repo.
//
// Run from the repository root:
//
//	go run ./cmd/iqfixture "F:\Dev\FMTAssistant\IQ Recordings"
//
// It looks at every .wav in the folder (with its .json beside it) and uses
// the first one that covers each key-down.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/va3mw/FMTAssistant/internal/iq"
)

type run struct {
	ID        string  `json:"id"`
	Title     string  `json:"title"`
	KeyDown   string  `json:"key_down"` // UTC
	Published float64 `json:"published_hz"`
}

// The published frequencies only centre a ±20 Hz search; the carrier is
// measured from the recording.
var runs = []run{
	{"run1", "W8RKO 40 m", "2026-11-13T02:34:00Z", 7063903.300},
	{"run2", "K5CM 40 m", "2026-11-13T02:49:00Z", 7065124.320},
	{"run3", "W8RKO 80 m", "2026-11-13T03:04:00Z", 3598877.400},
	{"run4", "K5CM 80 m", "2026-11-13T03:19:00Z", 3599325.170},
}

type result struct {
	run
	File     string       `json:"file"`
	Rate     int          `json:"rate"`
	DialHz   int64        `json:"dial_hz"`
	Mirrored bool         `json:"mirrored"`
	Segments []iq.Segment `json:"segments"`
	Summary  *iq.Summary  `json:"summary"`
	Notes    []string     `json:"notes"`
	Error    string       `json:"error,omitempty"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, `usage: go run ./cmd/iqfixture "<folder with the IQ .wav files>" [output.json]`)
		os.Exit(2)
	}
	out := filepath.Join("internal", "fmtdata", "testdata", "fmt2026-11", "iq_segments.json")
	if len(os.Args) > 2 {
		out = os.Args[2]
	}
	wavs, _ := filepath.Glob(filepath.Join(os.Args[1], "*.wav"))
	if len(wavs) == 0 {
		fmt.Fprintln(os.Stderr, "no .wav files in", os.Args[1])
		os.Exit(1)
	}
	var results []result
	for _, r := range runs {
		kd, _ := time.Parse(time.RFC3339, r.KeyDown)
		res := result{run: r, Error: "no recording covers this key-down"}
		for _, w := range wavs {
			var meta *iq.Meta
			if m, ok, err := iq.LoadMeta(w); err == nil && ok {
				meta = &m
			}
			a, err := iq.Analyze(w, meta, iq.Options{ExpectHz: r.Published, SearchHz: 20,
				From: kd, To: kd.Add(time.Minute), OnlyWindow: true})
			if err != nil {
				fmt.Printf("%s %s: %v\n", r.ID, filepath.Base(w), err)
				continue
			}
			res = result{run: r, File: filepath.Base(w), Rate: a.Rate, DialHz: a.DialHz, Mirrored: a.Mirrored,
				Segments: a.Segments, Summary: a.Summary, Notes: a.Notes}
			break
		}
		if res.Summary != nil {
			fmt.Printf("%s %s: %d segments, mean audio %.3f Hz, SD %.3f Hz, from %s\n",
				r.ID, r.Title, res.Summary.N, res.Summary.MeanAudio, res.Summary.SD, res.File)
		} else {
			fmt.Printf("%s %s: %s\n", r.ID, r.Title, res.Error)
		}
		results = append(results, res)
	}
	b, _ := json.MarshalIndent(results, "", " ")
	if err := os.WriteFile(out, b, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("wrote", out)
}
