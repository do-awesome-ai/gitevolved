# gitevolved

> **git that merges by what changed, not by text lines** — so non-overlapping edits
> to the same file merge automatically, no conflict. Your team and your AI agents
> work on the same code at once and stay in sync. Still git: `git push origin`
> works exactly as today. Free, open-source, Apache-2.0. — [gitevolved.ai](https://gitevolved.ai) · [docs](https://docs.gitevolved.ai)

## Why

Git merges text line-by-line, so it flags a conflict whenever two edits land near
each other — **even when they don't actually clash.** You change the top of a file,
your teammate changes the bottom, and git still makes someone untangle it by hand.
Now add AI: one developer runs several coding agents, all editing the same repo,
faster than anyone can merge them.

gitevolved records what you **did** — added a function, renamed a symbol, rewrote a
block — as a structured operation, not just which lines moved. Edits to different
parts of a file are different operations, so they combine cleanly and both land.
Edits that genuinely change the *same* code still ask a human — gitevolved just
stops inventing the conflicts that never needed to happen. Underneath it shells out
to your real, installed `git` (never forks it), so `clone`/`commit`/`push` behave as
you expect and `git push origin` to GitHub is untouched. Depends only on the Go
standard library.

## Install

**Let your AI agent do it** — paste to Claude Code / Codex / Gemini / any coding agent:

> Install gitevolved from github.com/do-awesome-ai/gitevolved, then make sure `git clone dosource://cloud/<repo>` works.

It reads [`AGENTS.md`](./AGENTS.md) and runs the steps. Or do it yourself:

```sh
# One command per tool (needs Go 1.26+). No node, no npm, no cloud account.
go install github.com/do-awesome-ai/gitevolved/cmd/git-remote-dosource@latest
go install github.com/do-awesome-ai/gitevolved/cmd/gitevolved@latest
go install github.com/do-awesome-ai/gitevolved/cmd/dosourced@latest
export PATH="$(go env GOPATH)/bin:$PATH"   # if not already on PATH
```

No Go? Download the prebuilt archive for your OS/arch from the
[latest release](https://github.com/do-awesome-ai/gitevolved/releases/latest) and
put the three binaries on PATH. Or clone + build: `./install.sh` (`--uninstall` to revert).

`git-remote-dosource` must keep that exact filename on `PATH` — git derives the
remote-helper from the `dosource://` scheme.

## Quickstart

```sh
# Or build the three binaries by hand (Go 1.26+; no node, no npm, no cloud account).
go build -o ~/bin/gitevolved          ./cmd/gitevolved
go build -o ~/bin/dosourced           ./cmd/dosourced
go build -o ~/bin/git-remote-dosource ./cmd/git-remote-dosource   # name matters: git derives it from the scheme
export PATH="$HOME/bin:$PATH"

# 1. Record edits as typed operations into a local op-log (no cloud).
cd my-project
gitevolved record --log .dosource/ops.jsonl main.go pkg/util.go
#   …or run the daemon and just edit files — every save is recorded automatically:
dosourced --root . --log .dosource/ops.jsonl &

# 2. See the current projected tree, or the operation history.
gitevolved materialize --log .dosource/ops.jsonl
gitevolved log         --log .dosource/ops.jsonl

# 3. It's a git remote. Clone the local op-log into a real git repo:
git clone dosource://$PWD/.dosource/ops.jsonl /tmp/exported
#   …and push an existing git repo's history INTO an op-log:
git -C some-repo push dosource://$PWD/.dosource/ops.jsonl main

# 4. Clone a HOSTED repo straight from the doSource cloud (on-ramp #1):
export DOSOURCE_API_KEY=...           # from `do login` / `do source attach`
git clone dosource://cloud/<repo> my-checkout

# 5. Push a local repo UP to the hosted cloud (creates the repo if new):
git -C my-checkout push dosource://cloud/<repo> main
```

Steps 1–3 are offline and single-machine. Step 4 reaches the hosted cloud over
plain HTTPS (no AWS account, no fork). Multi-machine/team coordination is the
(paid) cloud referee — see *What's NOT here*.

## Commands

| Binary | What it does |
|---|---|
| `gitevolved` | The local engine CLI: `record` (upsert files → ops), `rm` (record deletions), `materialize` (project the op-log to files), `export` (mint a real git commit), `log` (show the op-log). |
| `dosourced` | The auto-record daemon: watches a working tree and turns every file change into a typed operation in the op-log. Pure-stdlib polling watcher; no third-party deps. |
| `git-remote-dosource` | git's remote-helper for the `dosource://` scheme. Once on `PATH`, `git clone` / `git fetch` / `git push` against a `dosource://<op-log>` URL work like any git remote. |

## What's in here

Pure packages, each cloud-free and offline (Go standard library only):

| Package | What it does |
|---|---|
| `pkg/operation` | The typed-operation union — `AddFunction`, `RenameSymbol`, `EditStatement`, `RewriteRegion`, … An edit is a structured operation, not a line range. The `Envelope` (content-addressed `Seal`) is the op-log entry unit. |
| `pkg/extract` | Turns a `(path, pre, post)` content pair into a typed operation. |
| `pkg/projector` | Projects an append-only operation log into concrete files on demand (`go/parser` for Go; line-heuristic fallback elsewhere). |
| `pkg/conflict` | **Semantic conflict _detection_** — decides whether two operations are Independent, Ordered, Overlapping, or Conflicting at the symbol level. Two functions added to the same file are Independent, not a textual conflict. |
| `pkg/oplog` | The local append-only op-log: durable (fsync) JSONL persistence of sealed operation envelopes, fail-loud on a torn tail. |
| `pkg/localrepo` | The composition root — `RecordEdit`/`RecordFile`/`RecordDelete` / `Materialize` / `ExportToGit` over the primitives. The daemon + remote-helper are thin shells over it. |
| `pkg/watch` | Pure snapshot-diff change detector (modtime+size) the daemon polls. |
| `pkg/gitremote` | git's remote-helper protocol, both directions: import (op-log → `git fast-import` stream) and export (`git fast-export` stream → recorded ops). |
| `pkg/cloudsrc` | The cloud read+write backend. **Read:** materializes a hosted repo's tip tree (`commits`→`commit-detail`→`blob-urls`→presigned-S3 GET), so `git clone dosource://cloud/<repo>` works. **Write:** stages the pushed changes and commits them as one bundle (`attach`→`add` files + `add` DeleteFile typed-ops→`push`), so `git push dosource://cloud/<repo>` works — creating the repo if new, and on an incremental push correctly adding, modifying, AND deleting files. Stdlib HTTP only, no AWS SDK. |
| `pkg/export` | Projects state and writes a real **git** commit by shelling stock `git` via `os/exec`. Your `git log` / `git push origin` keep working unchanged. |
| `pkg/coord/*` | The open (in-memory, single-machine) halves of the coordination primitives — idempotency, merge queue, session state. The closed cloud referee implements the same exported contracts. |

## What's NOT here (and why)

Conflict **resolution**, the cloud coordination referee (CAS-fenced mainline,
merge queue, escalation cascade), team/multi-machine sync, and hosted storage
live in the closed doSource platform. This module is a clean leaf: the platform
imports it; it never imports the platform. That boundary is enforced by Go's
cross-module `internal/` visibility rule and by a CI guard.

## License & contributing

Apache-2.0 (see [`LICENSE`](./LICENSE) and [`NOTICE`](./NOTICE)) — a permissive
license with an explicit patent grant. Copyright 2026 do, inc.

Contributions are accepted under the [Developer Certificate of Origin](https://developercertificate.org)
(DCO): sign off every commit with `git commit -s`, which appends a
`Signed-off-by:` trailer certifying you wrote the patch or have the right to
submit it under Apache-2.0. See [`CONTRIBUTING.md`](./CONTRIBUTING.md) for the
full contribution guide (build/test, the DCO, and the open-client scope).
