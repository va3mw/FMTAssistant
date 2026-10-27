// Package session holds one FMT test: its schedule, the six measurement
// files, the operator's dial entries and the app settings.
package session

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

// Run is one carrier to measure.
type Run struct {
	ID         string    `json:"id"` // run1..run4
	Station    string    `json:"station"`
	Band       string    `json:"band"`
	NominalKHz float64   `json:"nominal_khz"` // published "near" frequency
	KeyDown    time.Time `json:"key_down"`    // UTC; call-up is 5 min before, key-up 1 min after
	// CarrierHz is the operator's best estimate of the carrier, used only to
	// suggest the dial. It starts at the nominal frequency.
	CarrierHz   float64      `json:"carrier_hz"`
	DialText    string       `json:"dial_text"`
	DialHz      float64      `json:"dial_hz"`
	RadioDialHz int64        `json:"radio_dial_hz"` // dial read from the Flex during the carrier
	Slot        Slot         `json:"slot"`
	MinSNR      float64      `json:"min_snr"`
	TrimEdges   bool         `json:"trim_edges"`
	Overrides   map[int]bool `json:"overrides"`
}

func (r *Run) CallUp() time.Time { return r.KeyDown.Add(-5 * time.Minute) }
func (r *Run) KeyUp() time.Time  { return r.KeyDown.Add(time.Minute) }

// Title is e.g. "W8RKO 40 m".
func (r *Run) Title() string { return strings.TrimSpace(r.Station + " " + r.Band) }

// Cal is a WWV calibration pass.
type Cal struct {
	ID       string       `json:"id"` // cal_before, cal_after
	Slot     Slot         `json:"slot"`
	MinSNR   float64      `json:"min_snr"`
	Stations map[int]bool `json:"stations"`
}

// Slot is a measurement file stored in the session folder.
type Slot struct {
	File       string `json:"file"`        // name inside the session folder
	Source     string `json:"source"`      // where it came from
	CapturedAt string `json:"captured_at"` // UTC, RFC 3339
}

type Flex struct {
	Enabled  bool   `json:"enabled"`
	Serial   string `json:"serial"`
	IP       string `json:"ip"` // manual IP, or last one seen
	Station  string `json:"station"`
	Slice    string `json:"slice"`     // slice letter
	AutoTune bool   `json:"auto_tune"` // tune at call-up
}

type Session struct {
	Version   int    `json:"version"`
	Name      string `json:"name"`
	Callsign  string `json:"callsign"`
	WSJTXDir  string `json:"wsjtx_dir"`
	CalBefore Cal    `json:"cal_before"`
	CalAfter  Cal    `json:"cal_after"`
	Runs      []Run  `json:"runs"`
	// CalMode picks the correction: "before", "after", "average" or "none".
	CalMode string `json:"cal_mode"`
	Voice   bool   `json:"voice"`
	Volume  int    `json:"volume"`
	Beeps   bool   `json:"beeps"`
	Flex    Flex   `json:"flex"`

	dir string
}

const fileName = "session.json"

// Default is the November 2026 ARRL FMT.
func Default() *Session {
	kd := func(h, m int) time.Time { return time.Date(2026, 11, 13, h, m, 0, 0, time.UTC) }
	run := func(id, st, band string, khz float64, t time.Time) Run {
		return Run{ID: id, Station: st, Band: band, NominalKHz: khz, KeyDown: t,
			CarrierHz: khz * 1000, MinSNR: 20, TrimEdges: true}
	}
	return &Session{
		Version:   1,
		Name:      "FMT 2026-11-13",
		WSJTXDir:  DefaultWSJTXDir(),
		CalBefore: Cal{ID: "cal_before", MinSNR: 20},
		CalAfter:  Cal{ID: "cal_after", MinSNR: 20},
		Runs: []Run{
			run("run1", "W8RKO", "40 m", 7064, kd(2, 34)),
			run("run2", "K5CM", "40 m", 7065, kd(2, 49)),
			run("run3", "W8RKO", "80 m", 3598, kd(3, 4)),
			run("run4", "K5CM", "80 m", 3599, kd(3, 19)),
		},
		CalMode: "average",
		Voice:   true,
		Volume:  100,
		Beeps:   true,
	}
}

// Dir is the session folder.
func (s *Session) Dir() string { return s.dir }

func (s *Session) SetDir(d string) { s.dir = d }

// Path returns the full path of a file in the session folder.
func (s *Session) Path(name string) string {
	if name == "" {
		return ""
	}
	return filepath.Join(s.dir, name)
}

// Load reads dir/session.json.
func Load(dir string) (*Session, error) {
	b, err := os.ReadFile(filepath.Join(dir, fileName))
	if err != nil {
		return nil, err
	}
	s := Default()
	if err := json.Unmarshal(b, s); err != nil {
		return nil, fmt.Errorf("%s: %w", fileName, err)
	}
	s.dir = dir
	return s, nil
}

// Save writes session.json atomically.
func (s *Session) Save() error {
	if s.dir == "" {
		return fmt.Errorf("no session folder")
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	tmp := filepath.Join(s.dir, fileName+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, fileName))
}

// Run finds a run by id.
func (s *Session) Run(id string) *Run {
	for i := range s.Runs {
		if s.Runs[i].ID == id {
			return &s.Runs[i]
		}
	}
	return nil
}

// Cal finds a calibration by id.
func (s *Session) Cal(id string) *Cal {
	switch id {
	case "cal_before":
		return &s.CalBefore
	case "cal_after":
		return &s.CalAfter
	}
	return nil
}

// SlotFor returns the slot with this id (calibration or run).
func (s *Session) SlotFor(id string) *Slot {
	if c := s.Cal(id); c != nil {
		return &c.Slot
	}
	if r := s.Run(id); r != nil {
		return &r.Slot
	}
	return nil
}

// SlotFileName is the stored file name for a slot, e.g. "2_run1_W8RKO_40m.all".
func (s *Session) SlotFileName(id string) string {
	switch id {
	case "cal_before":
		return "1_calibration_before.all"
	case "cal_after":
		return "6_calibration_after.all"
	}
	for i, r := range s.Runs {
		if r.ID == id {
			t := strings.NewReplacer(" ", "", "/", "-", "\\", "-").Replace(r.Title())
			return fmt.Sprintf("%d_%s_%s.all", i+2, r.ID, t)
		}
	}
	return id + ".all"
}

// DocumentsDir is the user's Documents folder (honours OneDrive redirection).
func DocumentsDir() string {
	if p, err := windows.KnownFolderPath(windows.FOLDERID_Documents, 0); err == nil && p != "" {
		return p
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, "Documents")
}

// DefaultDir is Documents\FMT Assistant\<name>.
func DefaultDir(name string) string {
	return filepath.Join(DocumentsDir(), "FMT Assistant", SafeName(name))
}

// SafeName strips characters Windows does not allow in folder names.
func SafeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`<>:"/\|?*`, r) || r < 32 {
			return '-'
		}
		return r
	}, strings.TrimSpace(s))
	if s == "" {
		s = "FMT"
	}
	return s
}

// DefaultWSJTXDir is where WSJT-X keeps fmt.all for the default rig name.
func DefaultWSJTXDir() string {
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "WSJT-X")
}

// WSJTXDirs lists WSJT-X data folders, including "WSJT-X - <rig>" ones.
func WSJTXDirs() []string {
	m, _ := filepath.Glob(filepath.Join(os.Getenv("LOCALAPPDATA"), "WSJT-X*"))
	var out []string
	for _, d := range m {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			out = append(out, d)
		}
	}
	return out
}

// ParseDial reads a dial frequency typed in Hz, kHz or MHz. A unit may be
// given; otherwise the size of the number decides (HF only, 1.8-30 MHz).
func ParseDial(text string) (float64, error) {
	t := strings.ToLower(strings.TrimSpace(text))
	if t == "" {
		return 0, fmt.Errorf("empty")
	}
	t = strings.NewReplacer(",", "", " ", "", "_", "").Replace(t)
	mult := 0.0
	for _, u := range []struct {
		suf string
		m   float64
	}{{"mhz", 1e6}, {"khz", 1e3}, {"hz", 1}} {
		if strings.HasSuffix(t, u.suf) {
			mult, t = u.m, strings.TrimSuffix(t, u.suf)
			break
		}
	}
	v, err := strconv.ParseFloat(t, 64)
	if err != nil || v <= 0 || math.IsInf(v, 0) {
		return 0, fmt.Errorf("%q is not a number", text)
	}
	if mult == 0 {
		switch {
		case v < 100:
			mult = 1e6
		case v < 100000:
			mult = 1e3
		default:
			mult = 1
		}
	}
	hz := math.Round(v*mult*1000) / 1000
	if hz < 1.7e6 || hz > 30e6 {
		return 0, fmt.Errorf("%.3f kHz is outside the HF range; type the dial in kHz, e.g. 7062.500", hz/1000)
	}
	return hz, nil
}
