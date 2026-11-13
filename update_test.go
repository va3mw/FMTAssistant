package main

import "testing"

func TestVersionNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"0.3.9", "0.3.8", true},
		{"0.3.10", "0.3.9", true},
		{"v0.4.0", "0.3.8", true},
		{"0.3.8", "0.3.8", false},
		{"0.3.7", "0.3.8", false},
		{"0.3.8-test1", "0.3.8", false},
		{"1.0", "0.9.9", true},
	} {
		if got := versionNewer(c.a, c.b); got != c.want {
			t.Errorf("versionNewer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
