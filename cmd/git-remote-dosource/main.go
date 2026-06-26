// Command git-remote-dosource is git's remote-helper for the dosource:// scheme.
// git invokes it automatically — `git clone dosource://<op-log>` runs
// `git-remote-dosource origin dosource://<op-log>` and speaks the helper protocol
// to it on stdin/stdout. This binary wires a LOCAL op-log as the source behind the
// pure protocol engine in pkg/gitremote; the user/agent types pure git.
//
// # Scope
//
// Both directions, over either a LOCAL op-log or a hosted cloud repo. The
// dosource:// URL selects the backend: dosource://cloud/<repo> is a hosted repo;
// any other dosource://<path> is a local op-log file. clone/fetch import; push
// exports. OPEN component: stdlib + the open gitevolved packages only.
//
// Install: run ./install.sh (it puts this binary on PATH under the exact name
// `git-remote-dosource` that git derives from the scheme), then
// `git clone dosource://cloud/<repo>`.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/do-awesome-ai/gitevolved/pkg/cloudsrc"
	"github.com/do-awesome-ai/gitevolved/pkg/gitremote"
	"github.com/do-awesome-ai/gitevolved/pkg/localrepo"
	"github.com/do-awesome-ai/gitevolved/pkg/oplog"
)

// scheme is the URL scheme git derives this helper's name from (git-remote-X).
const scheme = "dosource://"

// cloudPrefix marks a cloud-repo URL: dosource://cloud/<repo>. Anything else is a
// local op-log path: dosource://<path> (dosource:///abs/path → /abs/path).
const cloudPrefix = "cloud/"

// defaultCloudBase is the doSource cloud API root (overridable via DOSOURCE_API_BASE).
const defaultCloudBase = "https://api.doapps.cloud/v1/source"

// version is the build version, stamped by install.sh via
// -ldflags "-X main.version=<VERSION>". "dev" for a plain `go build`.
var version = "dev"

func main() {
	// A human may run `git-remote-dosource --version`; git never does.
	if len(os.Args) == 2 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Println("git-remote-dosource " + version)
			return
		}
	}
	// git invokes: git-remote-dosource <remote-name> <url>
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "git-remote-dosource: usage: git-remote-dosource <remote> <url>")
		fmt.Fprintln(os.Stderr, "(git invokes this automatically for dosource:// remotes)")
		os.Exit(2)
	}
	src, err := resolveSource(os.Args[2])
	if err != nil {
		fmt.Fprintf(os.Stderr, "git-remote-dosource: %v\n", err)
		os.Exit(1)
	}
	if err := gitremote.Serve(os.Stdin, os.Stdout, src); err != nil {
		fmt.Fprintf(os.Stderr, "git-remote-dosource: %v\n", err)
		os.Exit(1)
	}
}

// resolveSource parses the dosource:// URL and returns the matching backend:
// dosource://cloud/<repo> → a cloud-read Source (clone/fetch from the hosted repo);
// dosource://<path>        → the local op-log (read + write/push).
func resolveSource(raw string) (gitremote.Source, error) {
	isCloud, value, err := parseTarget(raw)
	if err != nil {
		return nil, err
	}
	if isCloud {
		base, apiKey, kerr := resolveCloudAuth()
		if kerr != nil {
			return nil, kerr
		}
		// Tenant left empty: the server derives it from the bearer.
		return cloudsrc.New(base, apiKey, "", value, nil), nil
	}
	return &localSource{oplogPath: value}, nil
}

// parseTarget classifies a dosource:// URL: (true, repo) for dosource://cloud/<repo>,
// (false, path) for a local op-log dosource://<path>. Pure — no auth, no I/O.
func parseTarget(raw string) (isCloud bool, value string, err error) {
	if !strings.HasPrefix(raw, scheme) {
		return false, "", fmt.Errorf("not a dosource:// URL: %q", raw)
	}
	rest := strings.TrimPrefix(raw, scheme)
	if rest == "" {
		return false, "", fmt.Errorf("empty target in URL %q", raw)
	}
	if repo := strings.TrimPrefix(rest, cloudPrefix); repo != rest {
		if repo == "" {
			return false, "", fmt.Errorf("cloud URL %q missing a repo (use dosource://cloud/<repo>)", raw)
		}
		return true, repo, nil
	}
	return false, rest, nil
}

// resolveCloudAuth resolves the cloud API base + a doSource API key the way the
// doSource tooling does, using only stdlib (no closed imports): the key from
// DOSOURCE_API_KEY, else the apiKey field of a .vscode/dosource.json binding
// found by walking up from the cwd. base from DOSOURCE_API_BASE, else the default.
func resolveCloudAuth() (base, apiKey string, err error) {
	base = strings.TrimSpace(os.Getenv("DOSOURCE_API_BASE"))
	if base == "" {
		base = defaultCloudBase
	}
	if k := strings.TrimSpace(os.Getenv("DOSOURCE_API_KEY")); k != "" {
		return base, k, nil
	}
	if k := bindingAPIKey(); k != "" {
		return base, k, nil
	}
	return "", "", fmt.Errorf("no doSource API key — set DOSOURCE_API_KEY or run `do source attach` to create a .vscode/dosource.json binding")
}

// bindingAPIKey returns the apiKey from the nearest .vscode/dosource.json, walking
// up from the cwd. Returns "" if none is found or it has no apiKey.
func bindingAPIKey() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		b, rerr := os.ReadFile(filepath.Join(dir, ".vscode", "dosource.json"))
		if rerr == nil {
			var binding struct {
				APIKey string `json:"apiKey"`
			}
			if json.Unmarshal(b, &binding) == nil && binding.APIKey != "" {
				return binding.APIKey
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// localSource projects + records a local op-log file. It satisfies both halves of
// the backend: the import (clone) read side (Tree/Head) and the export (push) write
// side (RecordFile/RecordDelete) — so `git push dosource://` records the pushed
// tree into the op-log as typed ops. One cached localrepo.Repo keeps turn numbering
// monotonic across the writes in a single push; the helper is short-lived per git
// invocation, so there's no long-lived handle to manage.
type localSource struct {
	oplogPath string
	repo      *localrepo.Repo
}

// ensure lazily opens (once) the engine over the op-log.
func (s *localSource) ensure() (*localrepo.Repo, error) {
	if s.repo == nil {
		r, err := localrepo.Open(s.oplogPath, "dosource-remote")
		if err != nil {
			return nil, err
		}
		s.repo = r
	}
	return s.repo, nil
}

// Tree materializes the op-log to the current projected working tree.
func (s *localSource) Tree() (map[string][]byte, error) {
	repo, err := s.ensure()
	if err != nil {
		return nil, err
	}
	state, err := repo.Materialize()
	if err != nil {
		return nil, err
	}
	return state, nil
}

// RecordFile records pushed content (export/push side).
func (s *localSource) RecordFile(path string, content []byte) (bool, error) {
	repo, err := s.ensure()
	if err != nil {
		return false, err
	}
	return repo.RecordFile(path, content)
}

// RecordDelete records a pushed deletion (export/push side).
func (s *localSource) RecordDelete(path string) (bool, error) {
	repo, err := s.ensure()
	if err != nil {
		return false, err
	}
	return repo.RecordDelete(path)
}

// Head reports the tip commit metadata. The commit time is the latest emitted
// operation's timestamp (so a clone is reproducible from the log, not wall-clock);
// an empty log returns ok=false (clone of an empty repo).
func (s *localSource) Head() (gitremote.CommitInfo, bool, error) {
	log, err := oplog.Open(s.oplogPath)
	if err != nil {
		return gitremote.CommitInfo{}, false, err
	}
	envs, err := log.Envelopes()
	if err != nil {
		return gitremote.CommitInfo{}, false, err
	}
	if len(envs) == 0 {
		return gitremote.CommitInfo{}, false, nil
	}
	latest := envs[len(envs)-1].EmittedAt.UTC()
	return gitremote.CommitInfo{
		Committer: "gitevolved <noreply@gitevolved.ai>",
		WhenUnix:  latest.Unix(),
		TZ:        "+0000",
		Subject:   "doSource import",
	}, true, nil
}
