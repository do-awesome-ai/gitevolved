// Command gitevolved is the runnable seed of the gitevolved.ai local client: a
// thin CLI over the localrepo engine that lets a developer drive the four open
// primitives (extract → oplog → projector → export) by hand, with zero cloud.
//
// # Why this exists
//
// localrepo is the composition root, but a library can't be run. This binary is
// the smallest thing that proves the whole open pipeline works end-to-end on a
// laptop and gives the local daemon + git-remote-dosource helper a reference
// for how to drive the engine:
//
//	gitevolved record   <file>...                 an edit happened → append a typed op
//	gitevolved rm        <path>                    a file was deleted → append a DeleteFile op
//	gitevolved materialize [--out <dir>]           what does the working tree look like?
//	gitevolved export --git <repo> --subject <s>   mint a real git commit (git log / push origin work)
//	gitevolved log                                 show the op-log (turn, kind, op-id, session)
//
// Every subcommand takes --log <path> (the op-log JSONL file) and --session <id>
// (the identity stamped on emitted ops; defaults to "local").
//
// This is an OPEN component of the free local client. It
// imports only the open gitevolved packages + the Go standard library — no cloud,
// no AWS SDK, no do-core. The command logic is factored into testable funcs that
// take args + an io.Writer; main only wires os.Args and the exit code.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/do-awesome-ai/gitevolved/pkg/localrepo"
	"github.com/do-awesome-ai/gitevolved/pkg/oplog"
)

// version is the build version, stamped by install.sh via
// -ldflags "-X main.version=<VERSION>". "dev" for a plain `go build`.
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version", "--version", "-v":
		fmt.Println("gitevolved " + version)
		return
	}
	if err := run(os.Args[1], os.Args[2:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "gitevolved: %v\n", err)
		os.Exit(1)
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `gitevolved — local doSource client (free, offline, no cloud)

usage:
  gitevolved record      --log <path> [--session <id>] <file>...   record edits (upsert from disk)
  gitevolved rm          --log <path> [--session <id>] <path>...    record deletions
  gitevolved materialize --log <path> [--out <dir>]                 project the op-log to files
  gitevolved export      --log <path> --git <repo> [--subject <s>]  mint a real git commit
  gitevolved log         --log <path>                               show the op-log

The --log path is the append-only op-log (JSONL). Paths passed to record/rm are
used verbatim as the in-repo tree path.
`)
}

// run dispatches a subcommand. Factored out of main so tests can drive it with an
// args slice + buffers and assert on output without spawning a process.
func run(cmd string, args []string, stdout, stderr io.Writer) error {
	switch cmd {
	case "record":
		return cmdRecord(args, stdout)
	case "rm":
		return cmdRm(args, stdout)
	case "materialize":
		return cmdMaterialize(args, stdout)
	case "export":
		return cmdExport(args, stdout)
	case "log":
		return cmdLog(args, stdout)
	case "help", "-h", "--help":
		usage(stdout)
		return nil
	default:
		usage(stderr)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// openRepo parses the --log/--session flags shared by the mutating commands and
// opens the engine. Returns the leftover positional args (the file/path list).
func openRepo(fs *flag.FlagSet, args []string) (*localrepo.Repo, []string, error) {
	logPath := fs.String("log", "", "path to the op-log JSONL file (required)")
	session := fs.String("session", "local", "session identity stamped on emitted operations")
	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}
	if *logPath == "" {
		return nil, nil, fmt.Errorf("--log is required")
	}
	repo, err := localrepo.Open(*logPath, *session)
	if err != nil {
		return nil, nil, err
	}
	return repo, fs.Args(), nil
}

func cmdRecord(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("record", flag.ContinueOnError)
	repo, files, err := openRepo(fs, args)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("record: no files given")
	}
	for _, f := range files {
		key, kerr := treeKey(f)
		if kerr != nil {
			return fmt.Errorf("record: %w", kerr)
		}
		content, rerr := os.ReadFile(f)
		if rerr != nil {
			return fmt.Errorf("record %s: %w", f, rerr)
		}
		recorded, rerr := repo.RecordFile(key, content)
		if rerr != nil {
			return rerr
		}
		if recorded {
			fmt.Fprintf(out, "recorded %s (turn %d)\n", key, repo.Turn())
		} else {
			fmt.Fprintf(out, "no change: %s\n", key)
		}
	}
	return nil
}

func cmdRm(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("rm", flag.ContinueOnError)
	repo, paths, err := openRepo(fs, args)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return fmt.Errorf("rm: no paths given")
	}
	for _, p := range paths {
		key, kerr := treeKey(p)
		if kerr != nil {
			return fmt.Errorf("rm: %w", kerr)
		}
		recorded, rerr := repo.RecordDelete(key)
		if rerr != nil {
			return rerr
		}
		if recorded {
			fmt.Fprintf(out, "removed %s (turn %d)\n", key, repo.Turn())
		} else {
			fmt.Fprintf(out, "not tracked: %s\n", key)
		}
	}
	return nil
}

// treeKey normalizes a path argument into the in-repo tree key the engine stores:
// a cleaned, relative, forward-slash path. Absolute paths are rejected loud (the
// export projector forbids them as path-traversal-unsafe, and the tree is meant
// to be repo-relative) — run from your repo root and pass relative paths, the way
// `git add` works.
func treeKey(arg string) (string, error) {
	if filepath.IsAbs(arg) {
		return "", fmt.Errorf("absolute path %q: pass a path relative to your repo root (run from the repo, like `git add`)", arg)
	}
	key := filepath.ToSlash(filepath.Clean(arg))
	if key == "." || strings.HasPrefix(key, "../") {
		return "", fmt.Errorf("path %q escapes the repo root", arg)
	}
	return key, nil
}

func cmdMaterialize(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("materialize", flag.ContinueOnError)
	logPath := fs.String("log", "", "path to the op-log JSONL file (required)")
	outDir := fs.String("out", "", "if set, write the projected tree under this directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *logPath == "" {
		return fmt.Errorf("--log is required")
	}
	repo, err := localrepo.Open(*logPath, "local")
	if err != nil {
		return err
	}
	state, err := repo.Materialize()
	if err != nil {
		return err
	}
	paths := make([]string, 0, len(state))
	for p := range state {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	if *outDir == "" {
		for _, p := range paths {
			fmt.Fprintf(out, "%s\t%d bytes\n", p, len(state[p]))
		}
		return nil
	}
	for _, p := range paths {
		dest := filepath.Join(*outDir, p)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fmt.Errorf("materialize %s: %w", p, err)
		}
		if err := os.WriteFile(dest, state[p], 0o644); err != nil {
			return fmt.Errorf("materialize %s: %w", p, err)
		}
		fmt.Fprintf(out, "wrote %s\n", dest)
	}
	return nil
}

func cmdExport(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	logPath := fs.String("log", "", "path to the op-log JSONL file (required)")
	gitRepo := fs.String("git", "", "path to a git-init'd repo to write the commit into (required)")
	subject := fs.String("subject", "dosource: export", "commit-message subject line")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *logPath == "" {
		return fmt.Errorf("--log is required")
	}
	if *gitRepo == "" {
		return fmt.Errorf("--git is required")
	}
	repo, err := localrepo.Open(*logPath, "local")
	if err != nil {
		return err
	}
	sha, err := repo.ExportToGit(*gitRepo, *subject)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, sha)
	return nil
}

func cmdLog(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	logPath := fs.String("log", "", "path to the op-log JSONL file (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *logPath == "" {
		return fmt.Errorf("--log is required")
	}
	log, err := oplog.Open(*logPath)
	if err != nil {
		return err
	}
	envs, err := log.Envelopes()
	if err != nil {
		return err
	}
	for _, env := range envs {
		opid := env.OpID
		if len(opid) > 12 {
			opid = opid[:12]
		}
		fmt.Fprintf(out, "turn %-4d %-16s %-12s %s\n", env.SourceTurn, env.OpType, opid, env.SourceSession)
	}
	return nil
}
