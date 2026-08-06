package stack

import "testing"

func TestParseMem(t *testing.T) {
	// The shapes docker actually prints, including the ones that differ by one letter.
	cases := []struct {
		in   string
		want int64
	}{
		{"1.234GiB / 7.653GiB", 1324997410},
		{"512MiB / 7.653GiB", 536870912},
		{"9.5KiB / 7.653GiB", 9728},
		{"1GB / 2GB", 1000000000},
		{"0B / 0B", 0},
		{"", 0},
		{"garbage", 0},
	}
	for _, c := range cases {
		if got := parseMem(c.in); got != c.want {
			t.Errorf("parseMem(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParsePercent(t *testing.T) {
	for in, want := range map[string]float64{"12.34%": 12.34, " 0.00% ": 0, "": 0, "--": 0} {
		if got := parsePercent(in); got != want {
			t.Errorf("parsePercent(%q) = %v, want %v", in, got, want)
		}
	}
}
