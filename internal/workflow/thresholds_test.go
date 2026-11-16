package workflow

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Every threshold has a line in thresholds.ini with a comment right above it.
func TestThresholdsDocumented(t *testing.T) {
	lines := strings.Split(DefaultINI(), "\n")
	typ := reflect.TypeOf(Thresholds{})
	for i := 0; i < typ.NumField(); i++ {
		key := typ.Field(i).Tag.Get("ini")
		found := false
		for j, l := range lines {
			if k, _, ok := strings.Cut(l, "="); ok && strings.TrimSpace(k) == key {
				found = true
				if j == 0 || !strings.HasPrefix(strings.TrimSpace(lines[j-1]), "#") {
					t.Errorf("%s has no comment above it", key)
				}
			}
		}
		if !found {
			t.Errorf("%s is missing from thresholds.ini", key)
		}
	}
	d := Defaults()
	v := reflect.ValueOf(d)
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).IsZero() {
			t.Errorf("%s defaults to zero", typ.Field(i).Name)
		}
	}
}

func TestLoadThresholds(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "thresholds.ini")
	th, err := LoadThresholds(p)
	if err != nil || th != Defaults() {
		t.Fatalf("missing file: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal("defaults not written for editing")
	}
	os.WriteFile(p, []byte("# mine\r\nagree_ppb = 8  # wider\r\n"), 0o644)
	th, err = LoadThresholds(p)
	if err != nil || th.AgreePPB != 8 || th.KeyDownSec != 60 {
		t.Fatalf("override: %v %+v", err, th)
	}
	os.WriteFile(p, []byte("agre_ppb = 8\n"), 0o644)
	if th, err = LoadThresholds(p); err == nil || th != Defaults() {
		t.Fatalf("unknown key accepted: %v", err)
	}
}
