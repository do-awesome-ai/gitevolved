<!-- CONTRIBUTING.md — how to contribute to gitevolved, and the DCO sign-off
     requirement. The canonical home for the contribution policy the README
     points at. -->

# Contributing to gitevolved

Thanks for your interest in `gitevolved` — the free, Apache-2.0 local client of
doSource. Contributions are welcome.

## Developer Certificate of Origin (DCO)

This project uses the [Developer Certificate of Origin](https://developercertificate.org)
(DCO) instead of a CLA. The DCO is a lightweight statement that you wrote the
patch — or otherwise have the right to submit it under Apache-2.0.

**Sign off every commit** by adding a `Signed-off-by` trailer:

```sh
git commit -s -m "your message"
```

That appends a line like:

```
Signed-off-by: Your Name <you@example.com>
```

using your `git config user.name` / `user.email`. By signing off you certify the
DCO (full text at the link above). Unsigned commits can't be merged.

## Building & testing

```sh
go build ./...
go test ./...
```

The module depends only on the Go standard library (and stock `git` on your PATH
for the export/remote-helper paths). No node, no npm, no cloud account is needed
to build or test.

## Scope — what lives here

`gitevolved` is the **open client**: the typed-operation engine, the projector,
the semantic conflict detector, the local op-log + daemon, the git remote-helper,
and the local-to-git export path. It shells out to stock `git` via `os/exec` and
**never** links git internals (so there is no GPL contagion).

The **cloud coordination referee** — the CAS-fenced shared mainline, the merge
queue, hosted storage, orgs/billing — is a separate hosted service and is **not**
part of this repository. Contributions here should stay within the open client:
the operation vocabulary, projection/conflict fidelity (especially adding
statement-level support for more languages), the watcher, and the git interop.

## Style

Match the surrounding code. Run `gofmt` (CI checks formatting). Every new file
opens with a short doc comment explaining what it is and why it exists.
