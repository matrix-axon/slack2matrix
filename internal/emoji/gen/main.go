// Command gen converts iamcal/emoji-data's emoji.json into the compact
// shortcode table embedded by package emoji.
//
//	go run ./internal/emoji/gen -in emoji.json -out internal/emoji/data.json
//
// emoji.json comes from https://cdn.jsdelivr.net/npm/emoji-datasource@15.1.2/emoji.json (MIT).
package main

import (
	"encoding/json"
	"flag"
	"log"
	"os"
	"sort"
)

type source struct {
	Unified        string   `json:"unified"`
	ShortNames     []string `json:"short_names"`
	SkinVariations map[string]struct {
		Unified string `json:"unified"`
	} `json:"skin_variations"`
}

// Entry is one shortcode's code points (hyphen-separated hex) plus its
// single-tone skin variations keyed by modifier code point.
type Entry struct {
	U string            `json:"u"`
	S map[string]string `json:"s,omitempty"`
}

func main() {
	in := flag.String("in", "emoji.json", "iamcal emoji.json")
	out := flag.String("out", "internal/emoji/data.json", "output file")
	flag.Parse()
	data, err := os.ReadFile(*in)
	if err != nil {
		log.Fatal(err)
	}
	var src []source
	if err := json.Unmarshal(data, &src); err != nil {
		log.Fatal(err)
	}
	table := map[string]Entry{}
	for _, e := range src {
		entry := Entry{U: e.Unified}
		for tone, v := range e.SkinVariations {
			// Multi-tone keys like "1F3FB-1F3FC" can't be expressed in Slack shortcodes.
			if len(tone) == 5 {
				if entry.S == nil {
					entry.S = map[string]string{}
				}
				entry.S[tone] = v.Unified
			}
		}
		for _, n := range e.ShortNames {
			table[n] = entry
		}
	}
	names := make([]string, 0, len(table))
	for n := range table {
		names = append(names, n)
	}
	sort.Strings(names)
	// Write deterministically so regenerating produces a stable diff.
	f, err := os.Create(*out)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	f.WriteString("{\n")
	for i, n := range names {
		k, _ := json.Marshal(n)
		v, _ := json.Marshal(table[n])
		f.Write(k)
		f.WriteString(":")
		f.Write(v)
		if i < len(names)-1 {
			f.WriteString(",")
		}
		f.WriteString("\n")
	}
	f.WriteString("}\n")
}
