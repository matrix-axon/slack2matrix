// Package emoji maps Slack emoji shortcodes to Unicode.
package emoji

import (
	_ "embed"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
)

//go:generate go run ./gen -in emoji.json -out data.json

//go:embed data.json
var rawData []byte

type entry struct {
	U string            `json:"u"`
	S map[string]string `json:"s"`
}

var (
	loadOnce sync.Once
	table    map[string]entry
)

// Slack-specific names that are absent from emoji-data.
var aliases = map[string]string{
	"simple_smile": "1F642",
	"facepalm":     "1F926",
	"thumbsup_all": "1F44D",
}

func load() {
	loadOnce.Do(func() {
		if err := json.Unmarshal(rawData, &table); err != nil {
			panic("emoji: corrupt embedded table: " + err.Error())
		}
		for name, u := range aliases {
			if _, ok := table[name]; !ok {
				table[name] = entry{U: u}
			}
		}
	})
}

// skinToneModifiers maps Slack's skin-tone-2..6 to the Fitzpatrick modifiers.
var skinToneModifiers = map[int]string{2: "1F3FB", 3: "1F3FC", 4: "1F3FD", 5: "1F3FE", 6: "1F3FF"}

// SplitName separates a Slack emoji name such as "+1::skin-tone-3" into the
// base name and skin tone (0 if none).
func SplitName(name string) (base string, skinTone int) {
	name = strings.Trim(name, ":")
	base, rest, found := strings.Cut(name, "::")
	if !found {
		return base, 0
	}
	if tone, ok := strings.CutPrefix(strings.Trim(rest, ":"), "skin-tone-"); ok {
		skinTone, _ = strconv.Atoi(tone)
	}
	return base, skinTone
}

// Lookup returns the Unicode emoji for a Slack shortcode (without colons),
// applying the skin tone if the emoji supports it. ok is false for names
// that aren't standard emoji, such as workspace custom emoji.
func Lookup(name string, skinTone int) (string, bool) {
	load()
	base, tone := SplitName(name)
	if skinTone == 0 {
		skinTone = tone
	}
	e, ok := table[base]
	if !ok {
		return "", false
	}
	code := e.U
	if mod, ok := skinToneModifiers[skinTone]; ok {
		if v, ok := e.S[mod]; ok {
			code = v
		}
	}
	return FromHex(code), true
}

// FromHex converts hyphen-separated hex code points ("1F44D-1F3FB") to a string.
func FromHex(code string) string {
	var sb strings.Builder
	for _, part := range strings.Split(code, "-") {
		cp, err := strconv.ParseUint(part, 16, 32)
		if err != nil {
			return ""
		}
		sb.WriteRune(rune(cp))
	}
	return sb.String()
}

// Render returns the Unicode emoji for a Slack name, or ":name:" if the emoji
// is custom or unknown.
func Render(name string, skinTone int) string {
	if s, ok := Lookup(name, skinTone); ok {
		return s
	}
	base, _ := SplitName(name)
	return ":" + base + ":"
}
