package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"slack2matrix/internal/config"
)

type channelStats struct {
	name          string
	archived      bool
	messages      int
	threadRoots   int
	threadReplies int
	reactions     int
	files         int
	missingFiles  int
	bytes         int64
}

func runPlan(_ context.Context, cfg *config.Config, _ string, args []string) error {
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	showMissing := fs.Bool("missing", false, "list files referenced by messages but missing on disk")
	_ = fs.Parse(args)

	exp, dir, _, _, err := loadTransformer(cfg)
	if err != nil {
		return err
	}
	subtypes := map[string]int{}
	modes := map[string]int{}
	var all []channelStats
	var total channelStats
	var missing []string
	for _, ch := range exp.Channels {
		msgs, err := exp.Messages(ch)
		if err != nil {
			return err
		}
		s := channelStats{name: ch.Name, archived: ch.IsArchived, messages: len(msgs)}
		for _, m := range msgs {
			st := m.Subtype
			if st == "" {
				st = "(none)"
			}
			subtypes[st]++
			if m.ReplyCount > 0 {
				s.threadRoots++
			}
			if m.IsThreadReply() {
				s.threadReplies++
			}
			for _, r := range m.Reactions {
				s.reactions += len(r.Users)
			}
			for i := range m.Files {
				f := &m.Files[i]
				s.files++
				modes[f.Mode]++
				if f.Mode == "tombstone" || f.Mode == "external" || f.DownloadURL() == "" {
					continue
				}
				if p := exp.FilePath(f); p == "" {
					s.missingFiles++
					missing = append(missing, fmt.Sprintf("#%s %s %s %q", ch.Name, m.TS, f.ID, f.Name))
				} else {
					s.bytes += f.Size
				}
			}
		}
		all = append(all, s)
		total.messages += s.messages
		total.threadRoots += s.threadRoots
		total.threadReplies += s.threadReplies
		total.reactions += s.reactions
		total.files += s.files
		total.missingFiles += s.missingFiles
		total.bytes += s.bytes
	}
	sort.Slice(all, func(i, j int) bool { return all[i].messages > all[j].messages })

	fmt.Printf("Export: %s\n", exp.Dir)
	fmt.Printf("Users: %d   Channels: %d   Canvases: %d\n\n", len(exp.Users), len(exp.Channels), len(exp.Canvases))
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "channel\tmessages\troots\treplies\treactions\tfiles\tmissing\tMiB\t")
	for _, s := range all {
		name := s.name
		if s.archived {
			name += " (archived)"
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t\n", name, s.messages, s.threadRoots, s.threadReplies, s.reactions, s.files, s.missingFiles, s.bytes>>20)
	}
	fmt.Fprintf(tw, "TOTAL\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t\n", total.messages, total.threadRoots, total.threadReplies, total.reactions, total.files, total.missingFiles, total.bytes>>20)
	_ = tw.Flush()

	fmt.Println("\nIdentities (real = existing Matrix account; others are created as ghosts):")
	for _, ident := range dir.All() {
		kind := "ghost"
		if ident.Real {
			kind = "REAL"
		} else if ident.Bot {
			kind = "bot ghost"
		}
		fmt.Printf("  %-10s %-45s %s\n", kind, ident.MXID, ident.DisplayName)
	}

	fmt.Println("\nSubtypes:")
	printCounts(subtypes)
	fmt.Println("\nFile modes:")
	printCounts(modes)
	if *showMissing {
		fmt.Println("\nMissing files:")
		for _, m := range missing {
			fmt.Println("  " + m)
		}
	}
	return nil
}

func printCounts(m map[string]int) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return m[keys[i]] > m[keys[j]] })
	for _, k := range keys {
		fmt.Printf("  %-22s %d\n", k, m[k])
	}
}
