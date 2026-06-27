# Installing gitevolved (instructions for AI coding agents)

You are an AI coding agent (Claude Code, Codex, Gemini, Aider, etc.) and your user
asked you to **install gitevolved**. Follow this recipe exactly. It installs three
small Go binaries and makes `git clone dosource://cloud/<repo>` work. It does not
touch the user's existing git/GitHub setup.

## What you're installing

Three binaries:
- `git-remote-dosource` — git's remote-helper for the `dosource://` scheme (the
  essential one; **its filename must stay exactly `git-remote-dosource`** on PATH —
  git derives the helper name from the URL scheme).
- `gitevolved` — the local CLI.
- `dosourced` — the local auto-record daemon.

## Step 1 — pick the install method

Check whether Go is installed: run `go version`.

### Method A — Go is installed (preferred)
```sh
go install github.com/do-awesome-ai/gitevolved/cmd/git-remote-dosource@latest
go install github.com/do-awesome-ai/gitevolved/cmd/gitevolved@latest
go install github.com/do-awesome-ai/gitevolved/cmd/dosourced@latest
```
Then ensure the Go bin dir is on PATH (add to the user's shell rc if missing):
```sh
export PATH="$(go env GOPATH)/bin:$PATH"   # usually ~/go/bin
```

### Method B — no Go toolchain
Download the prebuilt archive for the user's OS/arch from the latest release and
put the three binaries on PATH. Detect OS/arch with `uname -s` / `uname -m`.
```sh
# example: macOS arm64 (Apple Silicon). swap the asset name for the user's platform.
#   darwin_amd64 | darwin_arm64 | linux_amd64 | linux_arm64 | windows_amd64
ASSET="gitevolved_0.1.0_darwin_arm64.tar.gz"
curl -fsSL -o /tmp/gv.tgz "https://github.com/do-awesome-ai/gitevolved/releases/latest/download/${ASSET}"
mkdir -p "$HOME/.local/bin" && tar -xzf /tmp/gv.tgz -C /tmp
cp /tmp/gitevolved_*/{gitevolved,dosourced,git-remote-dosource} "$HOME/.local/bin/"
export PATH="$HOME/.local/bin:$PATH"   # add to shell rc if missing
```
(Windows: download the `_windows_amd64.zip`, extract, put the three `.exe` on PATH.)

## Step 2 — verify the install
```sh
git-remote-dosource --version    # should print: git-remote-dosource <version>
```
If `git-remote-dosource` is not found, the install dir is not on PATH — fix PATH
and retry. Do not rename the binary.

## Step 3 — (only if the user wants the hosted cloud) set the API key
For `dosource://cloud/<repo>` URLs, the user needs a doSource API key. Set it in
the environment (or it is read from a `.vscode/dosource.json` `apiKey` field if
present):
```sh
export DOSOURCE_API_KEY="<the user's key>"
```
Ask the user for the key; do not invent one. Local op-log usage (Method A/B above)
needs no key — the cloud key is only for `dosource://cloud/...`.

## Step 4 — confirm it works
```sh
git clone dosource://cloud/<repo> my-checkout    # hosted repo, needs the key
# or, fully local / offline, no key:
gitevolved --version
```

## Notes for the agent
- This never modifies the user's normal git remotes. `git push origin` is untouched.
- Do not run anything as root. Install to the user's PATH (`~/go/bin` or
  `~/.local/bin`), not system dirs.
- If `go install` fails on network/proxy, fall back to Method B (prebuilt binary).
- Full docs: https://docs.gitevolved.ai
