package version

import "testing"

func TestCompare(t *testing.T) {
	t.Parallel()
	tests := []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0},
		{"v1.2.3", "1.2.3", 0},
		{"1.2.3", "1.10.0", -1},
		{"2.0.0", "1.99.99", 1},
		{"1.2.3-rc.1", "1.2.3", 0},
		{"dev", "1.0.0", 1},
		{"1.0.0", "dev", -1},
	}
	for _, tt := range tests {
		if got := Compare(tt.a, tt.b); got != tt.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}
