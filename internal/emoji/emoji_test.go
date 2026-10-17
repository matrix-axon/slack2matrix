package emoji

import "testing"

func TestRender(t *testing.T) {
	cases := map[string]string{
		"+1":                 "👍",
		"thumbsup":           "👍",
		"+1::skin-tone-6":    "👍🏿",
		"face_palm":          "🤦",
		"simple_smile":       "🙂",
		"allthethings":       ":allthethings:",
		"woman-gesturing-no": "🙅‍♀️",
	}
	for name, want := range cases {
		if got := Render(name, 0); got != want {
			t.Errorf("Render(%q) = %q, want %q", name, got, want)
		}
	}
}
