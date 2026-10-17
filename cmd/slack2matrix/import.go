package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/term"
	"maunium.net/go/mautrix/id"

	"slack2matrix/internal/config"
	"slack2matrix/internal/e2ee"
	"slack2matrix/internal/importer"
	"slack2matrix/internal/matrix"
	"slack2matrix/internal/slackexport"
	"slack2matrix/internal/store"
)

// newLogger logs to the console and to a file in the state directory.
func newLogger(cfg *config.Config, verbose bool) (zerolog.Logger, io.Closer, error) {
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return zerolog.Nop(), nil, err
	}
	f, err := os.OpenFile(filepath.Join(cfg.StateDir, "import.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return zerolog.Nop(), nil, err
	}
	console := zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.TimeOnly}
	level := zerolog.InfoLevel
	if verbose {
		level = zerolog.DebugLevel
	}
	consoleLevel := zerolog.MultiLevelWriter(levelWriter{console, level}, levelWriter{f, zerolog.DebugLevel})
	return zerolog.New(consoleLevel).Level(level).With().Timestamp().Logger(), f, nil
}

// levelWriter filters events below a minimum level for one destination.
type levelWriter struct {
	w   io.Writer
	min zerolog.Level
}

func (lw levelWriter) Write(p []byte) (int, error) { return lw.w.Write(p) }

func (lw levelWriter) WriteLevel(l zerolog.Level, p []byte) (int, error) {
	if l < lw.min {
		return len(p), nil
	}
	return lw.w.Write(p)
}

// openImporter connects everything needed to talk to Matrix.
func openImporter(ctx context.Context, cfg *config.Config, log zerolog.Logger) (*importer.Importer, *store.Store, error) {
	exp, dir, _, _, err := loadTransformer(cfg)
	if err != nil {
		return nil, nil, err
	}
	st, err := store.Open(cfg.StateDir)
	if err != nil {
		return nil, nil, err
	}
	mx, err := matrix.New(ctx, cfg, log)
	if err != nil {
		st.Close()
		return nil, nil, err
	}
	imp, err := importer.New(ctx, cfg, exp, dir, st, mx, log)
	if err != nil {
		st.Close()
		return nil, nil, err
	}
	return imp, st, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseDate(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse("2006-01-02", s)
}

// setupAll creates ghosts, the space and the rooms for the given channels.
func setupAll(ctx context.Context, imp *importer.Importer, channels []*slackexport.Channel) error {
	if err := imp.SetupUsers(ctx); err != nil {
		return err
	}
	space, err := imp.EnsureSpace(ctx)
	if err != nil {
		return err
	}
	for _, ch := range channels {
		if _, err := imp.EnsureRoom(ctx, space, ch); err != nil {
			return err
		}
	}
	return nil
}

func runSetup(ctx context.Context, cfg *config.Config, _ string, args []string) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	channels := fs.String("channels", "", "comma-separated channel names (default: all)")
	verbose := fs.Bool("v", false, "verbose logging")
	_ = fs.Parse(args)
	log, closer, err := newLogger(cfg, *verbose)
	if err != nil {
		return err
	}
	defer closer.Close()
	imp, st, err := openImporter(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer st.Close()
	chans, err := imp.SelectChannels(splitList(*channels))
	if err != nil {
		return err
	}
	return setupAll(ctx, imp, chans)
}

func runImport(ctx context.Context, cfg *config.Config, _ string, args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	channels := fs.String("channels", "", "comma-separated channel names (default: all)")
	since := fs.String("since", "", "only messages on or after this date (YYYY-MM-DD)")
	until := fs.String("until", "", "only messages before this date (YYYY-MM-DD)")
	limit := fs.Int("limit", 0, "maximum messages per channel (0 = all)")
	concurrency := fs.Int("concurrency", cfg.Import.RoomConcurrency, "channels imported in parallel")
	verbose := fs.Bool("v", false, "verbose logging")
	_ = fs.Parse(args)

	var f importer.Filter
	var err error
	if f.Since, err = parseDate(*since); err != nil {
		return err
	}
	if f.Until, err = parseDate(*until); err != nil {
		return err
	}
	f.Limit = *limit

	log, closer, err := newLogger(cfg, *verbose)
	if err != nil {
		return err
	}
	defer closer.Close()
	imp, st, err := openImporter(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer st.Close()
	chans, err := imp.SelectChannels(splitList(*channels))
	if err != nil {
		return err
	}
	if err := setupAll(ctx, imp, chans); err != nil {
		return err
	}

	// Each room is sent sequentially, so the largest channels bound the total
	// time: start them first.
	sizes := map[string]int64{}
	for _, ch := range chans {
		sizes[ch.ID] = imp.Exp.ChannelSize(ch)
	}
	sort.SliceStable(chans, func(i, j int) bool { return sizes[chans[i].ID] > sizes[chans[j].ID] })

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	work := make(chan *slackexport.Channel)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []string
	for range max(*concurrency, 1) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ch := range work {
				if err := imp.ImportChannel(ctx, ch, f); err != nil {
					log.Error().Err(err).Str("channel", ch.Name).Msg("Channel import failed")
					mu.Lock()
					failures = append(failures, ch.Name)
					mu.Unlock()
				}
			}
		}()
	}
	for _, ch := range chans {
		select {
		case work <- ch:
		case <-ctx.Done():
		}
	}
	close(work)
	wg.Wait()
	if len(failures) > 0 {
		return fmt.Errorf("%d channel(s) failed: %s (re-run to resume)", len(failures), strings.Join(failures, ", "))
	}
	return ctx.Err()
}

func runKeys(ctx context.Context, cfg *config.Config, _ string, args []string) error {
	fs := flag.NewFlagSet("keys", flag.ExitOnError)
	force := fs.Bool("force", false, "upload to key backups even if their signatures can't be verified")
	verbose := fs.Bool("v", false, "verbose logging")
	_ = fs.Parse(args)
	log, closer, err := newLogger(cfg, *verbose)
	if err != nil {
		return err
	}
	defer closer.Close()
	imp, st, err := openImporter(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer st.Close()
	return imp.DeliverKeys(ctx, *force)
}

func runFinalize(ctx context.Context, cfg *config.Config, _ string, args []string) error {
	fs := flag.NewFlagSet("finalize", flag.ExitOnError)
	force := fs.Bool("force", false, "upload to key backups even if their signatures can't be verified")
	verbose := fs.Bool("v", false, "verbose logging")
	_ = fs.Parse(args)
	log, closer, err := newLogger(cfg, *verbose)
	if err != nil {
		return err
	}
	defer closer.Close()
	imp, st, err := openImporter(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := imp.DeliverKeys(ctx, *force); err != nil {
		return err
	}
	return imp.Finalize(ctx)
}

// runCrossSign verifies a real user's import device with their recovery key,
// read from a file or typed at a prompt. The key is never stored.
func runCrossSign(ctx context.Context, cfg *config.Config, _ string, args []string) error {
	fs := flag.NewFlagSet("crosssign", flag.ExitOnError)
	user := fs.String("user", "", "the real Matrix user whose import device to verify")
	keyFile := fs.String("recovery-key-file", "", "file containing the recovery key (default: prompt)")
	verbose := fs.Bool("v", false, "verbose logging")
	_ = fs.Parse(args)
	if *user == "" {
		return errors.New("-user is required")
	}
	var key string
	var err error
	if *keyFile != "" {
		key, err = e2ee.ReadSecretFile(*keyFile)
	} else {
		key, err = promptSecret(fmt.Sprintf("Recovery key for %s: ", *user))
	}
	if err != nil {
		return err
	}
	log, closer, err := newLogger(cfg, *verbose)
	if err != nil {
		return err
	}
	defer closer.Close()
	imp, st, err := openImporter(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer st.Close()
	return imp.E2EE.CrossSignWithRecoveryKey(ctx, id.UserID(*user), key)
}

// promptSecret reads a line from the terminal without echoing it.
func promptSecret(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		return strings.TrimSpace(string(b)), err
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
