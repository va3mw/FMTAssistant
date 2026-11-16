// Package workflow holds the checks that guide an FMT session: capture
// length, calibration quality, fading, corrections and the analysis text.
// It has no Windows or UI dependencies, so it is tested on any platform.
package workflow

import (
	"bufio"
	_ "embed"
	"fmt"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
)

//go:embed thresholds.ini
var defaultINI string

// DefaultINI is the commented settings file with every default.
func DefaultINI() string { return defaultINI }

// Thresholds are the limits the checks use. Each field's ini tag names its
// line in thresholds.ini, where it is explained.
type Thresholds struct {
	KeyDownSec           float64 `ini:"key_down_sec"`
	ShortCaptureFraction float64 `ini:"short_capture_fraction"`
	HalfCycleLastSec     float64 `ini:"half_cycle_last_sec"`
	LiveCheckAfterSec    float64 `ini:"live_check_after_sec"`
	LiveCheckMinReadings int     `ini:"live_check_min_readings"`
	ForeignReadingsBlock int     `ini:"foreign_readings_block"`
	CalMinWWVFraction    float64 `ini:"cal_min_wwv_fraction"`
	CalDwellSec          float64 `ini:"cal_dwell_sec"`
	CalDwellShort        float64 `ini:"cal_dwell_short_fraction"`
	DwellGapSec          float64 `ini:"dwell_gap_sec"`

	CalMinReadings  int     `ini:"cal_min_readings"`
	CalNoisySD      float64 `ini:"cal_noisy_sd_hz"`
	CalSteadySD     float64 `ini:"cal_steady_sd_hz"`
	CalMinFreqs     int     `ini:"cal_min_freqs"`
	CalMinSpanRatio float64 `ini:"cal_min_span_ratio"`
	CalSlopeSigmas  float64 `ini:"cal_slope_sigmas"`

	SlopeConfidence float64 `ini:"slope_confidence"`
	FadingSDRatio   float64 `ini:"fading_sd_ratio"`
	FadingSDHz      float64 `ini:"fading_sd_fallback_hz"`
	FadingLag1      float64 `ini:"fading_lag1"`
	AutocorrNote    float64 `ini:"autocorr_note"`

	AgreePPB         float64 `ini:"agree_ppb"`
	BlunderHz        float64 `ini:"blunder_hz"`
	AudioClockSpread float64 `ini:"audio_clock_spread"`
	RefCheckOKPPB    float64 `ini:"refcheck_ok_ppb"`
}

// Defaults returns the thresholds in the built-in thresholds.ini.
func Defaults() Thresholds {
	var t Thresholds
	if err := t.parse(strings.NewReader(defaultINI)); err != nil {
		panic("thresholds.ini: " + err.Error())
	}
	return t
}

// LoadThresholds reads the operator's copy at path over the defaults. A
// missing file gives the defaults and writes them there for editing.
func LoadThresholds(path string) (Thresholds, error) {
	t := Defaults()
	fh, err := os.Open(path)
	if os.IsNotExist(err) {
		_ = os.WriteFile(path, []byte(strings.ReplaceAll(defaultINI, "\n", "\r\n")), 0o644)
		return t, nil
	}
	if err != nil {
		return t, err
	}
	defer fh.Close()
	if err := t.parse(fh); err != nil {
		return Defaults(), fmt.Errorf("%s: %w (using the defaults)", path, err)
	}
	return t, nil
}

func (t *Thresholds) parse(r io.Reader) error {
	fields := map[string]reflect.Value{}
	v := reflect.ValueOf(t).Elem()
	for i := 0; i < v.NumField(); i++ {
		fields[v.Type().Field(i).Tag.Get("ini")] = v.Field(i)
	}
	sc := bufio.NewScanner(r)
	n := 0
	for sc.Scan() {
		n++
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		k, val, ok := strings.Cut(s, "=")
		if !ok {
			return fmt.Errorf("line %d: expected name = value", n)
		}
		k, val = strings.TrimSpace(k), strings.TrimSpace(val)
		if i := strings.Index(val, "#"); i >= 0 {
			val = strings.TrimSpace(val[:i])
		}
		f, ok := fields[k]
		if !ok {
			return fmt.Errorf("line %d: unknown setting %q", n, k)
		}
		x, err := strconv.ParseFloat(val, 64)
		if err != nil || x < 0 {
			return fmt.Errorf("line %d: %s must be a number of 0 or more", n, k)
		}
		switch f.Kind() {
		case reflect.Int:
			f.SetInt(int64(x))
		default:
			f.SetFloat(x)
		}
	}
	return sc.Err()
}
