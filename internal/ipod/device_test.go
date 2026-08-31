package ipod

import "testing"

func TestIsClassic(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"MB029", true},  // 6G 80GB silver
		{"MB147", true},  // 6G 80GB black
		{"MB150", true},  // 6G 160GB black
		{"MB562", true},  // 7G 120GB
		{"MC293", true},  // 7G 160GB
		{"MC297", true},  // 7G 160GB
		{"MA002", false}, // 5G 30GB
		{"MA146", false}, // 5G 30GB
		{"MA448", false}, // 5.5G 80GB
		{"", false},      // unknown -> 5G default
		{"ZZ999", false}, // unknown -> 5G default
	}
	for _, c := range cases {
		if got := IsClassic(c.model); got != c.want {
			t.Errorf("IsClassic(%q) = %v, want %v", c.model, got, c.want)
		}
	}
}
