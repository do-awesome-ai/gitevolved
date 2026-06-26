# gitevolved

> A semantic, offline-first local engine for source control that understands your
> code as **typed operations**, not text diffs — the open-source front door to
> [doSource](https://gitevolved.ai).

`gitevolved` is the free, Apache-2.0-licensed local core of doSource. It runs
fully on your laptop, depends only on the Go standard library, and shells out to
**stock `git`** for interop — it never forks or links git internals, so there is
no GPL contagion. The headline: it teaches git a smarter transport so
`git clone dosource://…` and `git push dosource://…` Just Work, with **`git push
origin` left completely untouched.**

## Install

```sh
# One step: build + install all three binaries (Go 1.26+; no node, no npm, no
# cloud account). Installs to $HOME/.local/bin by default (--prefix to change).
./install.sh
# then add the prefix to PATH if it isn't already (the installer tells you):
export PATH="$HOME/.local/bin:$PATH"
# uninstall any time: ./install.sh --uninstall
```

`git-remote-dosource` must keep that exact filename on `PATH` — git derives the
remote-helper from the `dosource://` scheme. The installer handles this; if you
build by hand (below), keep the output name.

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
