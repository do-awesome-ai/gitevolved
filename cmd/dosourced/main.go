// Command dosourced is the gitevolved local daemon: it watches a working tree
// and turns every file change into a typed operation appended to the local
// op-log, with zero cloud. It is the automation layer over the same localrepo
// engine the gitevolved CLI drives by hand — "edit a file, an op is recorded"
// happens without you running `gitevolved record`.
//
// # Why this exists
//
// The CLI proves the pipeline; the daemon makes it ambient. Combined with
// `gitevolved export`, the local-only loop is complete and offline:
//
//	edit files → dosourced records ops → `gitevolved export` mints git commits
//
// # Design
//
// dosourced is a thin shell: a watch.Poller (pure stdlib snapshot diff) feeds
// changed/deleted paths to localrepo.RecordFile/RecordDelete on a ticker. The
// op-log file is excluded from the watch so the daemon never records its own
// writes. The record step is factored into recordChanges (no timers, no signals)
// so it is unit-tested directly; main only owns flags, the ticker, and clean
// shutdown on SIGINT/SIGTERM.
//
// The polling watcher is dependency-free by design (see pkg/watch); an
// event-driven backend can replace it behind the same seam without touching this
// file. This is an OPEN component of the free local client: stdlib only, no
// cloud, no do-core, no AWS SDK — no closed-source dependencies.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/do-awesome-ai/gitevolved/pkg/localrepo"
	"github.com/do-awesome-ai/gitevolved/pkg/watch"
)

// version is the build version, stamped by install.sh via
// -ldflags "-X main.version=<VERSION>". "dev" for a plain `go build`.
var version = "dev"

func main() {
	if len(os.Args) == 2 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Println("dosourced " + version)
			return
		}
	}
	root := flag.String("root", ".", "working-tree directory to watch")
	logPath := flag.String("log", "", "op-log JSONL path (default <root>/.dosource/ops.jsonl)")
	session := flag.String("session", "local", "session identity stamped on emitted operations")
	interval := flag.Duration("interval", time.Second, "poll interval")
	flag.Parse()

	resolvedLog := *logPath
	if resolvedLog == "" {
		resolvedLog = filepath.Join(*root, ".dosource", "ops.jsonl")
	}
	if err := os.MkdirAll(filepath.Dir(resolvedLog), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "dosourced: %v\n", err)
		os.Exit(1)
	}

	repo, err := localrepo.Open(resolvedLog, *session)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dosourced: %v\n", err)
		os.Exit(1)
	}
	poller := watch.NewPoller(*root, resolvedLog)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(os.Stdout, "dosourced: watching %s → %s (every %s)\n", *root, resolvedLog, *interval)
	if err := serve(ctx, poller, repo, *root, *interval, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "dosourced: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "dosourced: stopped")
}

// recorder is the slice of localrepo the daemon needs — declared as an interface
// so serve/recordChanges are testable against a fake and don't pull the whole
// concrete engine into a test that only cares about the record loop.
type recorder interface {
	RecordFile(path string, content []byte) (bool, error)
	RecordDelete(path string) (bool, error)
}

// serve runs the poll→record loop until ctx is cancelled, then does one final
// poll so edits made just before shutdown aren't lost. The ticker is a polling
// loop (a legitimate use of a timer), not sleep-to-coordinate.
func serve(ctx context.Context, poller *watch.Poller, repo recorder, root string, interval time.Duration, logw io.Writer) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// Final sweep on shutdown.
			return pollOnce(poller, repo, root, logw)
		case <-ticker.C:
			if err := pollOnce(poller, repo, root, logw); err != nil {
				return err
			}
		}
	}
}

// pollOnce performs a single poll and records the resulting changes. A walk error
// is fatal (the tree is unreadable); a per-file record error is logged and
// skipped so one bad file can't take the daemon down.
func pollOnce(poller *watch.Poller, repo recorder, root string, logw io.Writer) error {
	changed, deleted, err := poller.Poll()
	if err != nil {
		return fmt.Errorf("poll: %w", err)
	}
	recordChanges(repo, root, changed, deleted, logw)
	return nil
}

// recordChanges reads each changed file and records it, and records each deletion.
// Pure of timers/signals — the unit-test entry point. Per-file errors (a file
// that vanished between poll and read, an unreadable path, an extract failure)
// are logged and skipped, never returned, so the daemon keeps running.
func recordChanges(repo recorder, root string, changed, deleted []string, logw io.Writer) {
	for _, key := range changed {
		content, rerr := os.ReadFile(filepath.Join(root, filepath.FromSlash(key)))
		if rerr != nil {
			fmt.Fprintf(logw, "dosourced: skip %s: %v\n", key, rerr)
			continue
		}
		recorded, rerr := repo.RecordFile(key, content)
		if rerr != nil {
			fmt.Fprintf(logw, "dosourced: record %s: %v\n", key, rerr)
			continue
		}
		if recorded {
			fmt.Fprintf(logw, "dosourced: recorded %s\n", key)
		}
	}
	for _, key := range deleted {
		recorded, rerr := repo.RecordDelete(key)
		if rerr != nil {
			fmt.Fprintf(logw, "dosourced: delete %s: %v\n", key, rerr)
			continue
		}
		if recorded {
			fmt.Fprintf(logw, "dosourced: removed %s\n", key)
		}
	}
}
