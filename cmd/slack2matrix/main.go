// Command slack2matrix imports a Slack workspace export into end-to-end
// encrypted Matrix rooms via a Synapse appservice.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"syscall"

	"slack2matrix/internal/config"
)

type command struct {
	summary string
	// needsConfig commands get the parsed config; others receive nil.
	needsConfig bool
	run         func(ctx context.Context, cfg *config.Config, configPath string, args []string) error
}

var commands = map[string]command{
	"init":         {"write a config skeleton with fresh tokens", false, runInit},
	"plan":         {"print statistics about the export without touching Matrix", true, runPlan},
	"registration": {"write the Synapse appservice registration file", true, runRegistration},
	"render":       {"show the Matrix events a Slack message becomes", true, runRender},
	"setup":        {"create ghosts, the space and rooms", true, runSetup},
	"import":       {"import messages (runs setup first; resumable)", true, runImport},
	"keys":         {"upload keys to users' backups and write the key export file", true, runKeys},
	"finalize":     {"deliver keys, unmute rooms and mark them read (run after the import)", true, runFinalize},
	"crosssign":    {"verify a real user's import device with their recovery key", true, runCrossSign},
	"testclient":   {"log in as a test user's device and verify decryption", true, runTestClient},
}

func usage() {
	fmt.Fprintf(os.Stderr, "usage: slack2matrix [-config path] <command> [flags]\n\ncommands:\n")
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(os.Stderr, "  %-14s %s\n", n, commands[n].summary)
	}
}

func main() {
	configPath := flag.String("config", "config.yaml", "path to the config file")
	flag.Usage = usage
	flag.Parse()
	if flag.NArg() < 1 {
		usage()
		os.Exit(2)
	}
	cmd, ok := commands[flag.Arg(0)]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", flag.Arg(0))
		usage()
		os.Exit(2)
	}
	var cfg *config.Config
	if cmd.needsConfig {
		var err error
		if cfg, err = config.Load(*configPath); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cmd.run(ctx, cfg, *configPath, flag.Args()[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
