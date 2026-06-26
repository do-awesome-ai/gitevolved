#!/usr/bin/env bash
# install.sh — build + install the three gitevolved binaries onto your PATH.
#
# WHY THIS EXISTS: the "front door" on-ramp — discover, install, use in 60s.
# The quickstart's manual `go build` lines work, but a developer who just wants
# `git clone dosource://…` to Just Work shouldn't have to remember three build
# invocations and the fact that git derives the remote-helper's name from the
# scheme (so `git-remote-dosource` MUST keep that exact filename on PATH). This
# script does all three builds with the version stamped in, installs them to a
# prefix, and tells you if that prefix isn't on PATH.
#
# Usage:
#   ./install.sh                     # build + install to $HOME/.local/bin
#   ./install.sh --prefix ~/bin      # install to a different dir
#   ./install.sh --uninstall         # remove the three binaries from the prefix
#
# Idempotent: re-running rebuilds + overwrites. OPEN component — pure Go + git,
# no cloud account needed to install.
set -euo pipefail

# Resolve the module dir (this script lives at the module root).
MODULE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BINARIES=(gitevolved dosourced git-remote-dosource)

PREFIX="${HOME}/.local/bin"
UNINSTALL=0
while [ $# -gt 0 ]; do
	case "$1" in
		--prefix) PREFIX="$2"; shift 2 ;;
		--prefix=*) PREFIX="${1#--prefix=}"; shift ;;
		--uninstall) UNINSTALL=1; shift ;;
		-h|--help)
			grep '^#' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
			exit 0 ;;
		*) echo "install.sh: unknown argument: $1" >&2; exit 2 ;;
	esac
done

if [ "$UNINSTALL" = "1" ]; then
	removed=0
	for b in "${BINARIES[@]}"; do
		if [ -e "${PREFIX}/${b}" ]; then
			rm -f "${PREFIX}/${b}"
			echo "removed ${PREFIX}/${b}"
			removed=$((removed + 1))
		fi
	done
	[ "$removed" = "0" ] && echo "nothing to uninstall in ${PREFIX}"
	exit 0
fi

command -v go >/dev/null 2>&1 || {
	echo "install.sh: Go is required (install from https://go.dev/dl/)" >&2
	exit 1
}

VERSION="dev"
[ -f "${MODULE_DIR}/VERSION" ] && VERSION="$(tr -d '[:space:]' < "${MODULE_DIR}/VERSION")"

mkdir -p "$PREFIX"
echo "building gitevolved ${VERSION} → ${PREFIX}"
for b in "${BINARIES[@]}"; do
	# The version is stamped into each main's `version` var. git-remote-dosource
	# MUST keep its exact name: git derives the helper from the dosource:// scheme.
	( cd "$MODULE_DIR" && go build -ldflags "-X main.version=${VERSION}" -o "${PREFIX}/${b}" "./cmd/${b}" )
	echo "  installed ${b}"
done

# Verify the helper runs and reports its version (proves the install is sound).
"${PREFIX}/git-remote-dosource" --version >/dev/null || {
	echo "install.sh: installed git-remote-dosource failed to run" >&2
	exit 1
}

echo
echo "Installed: ${BINARIES[*]} → ${PREFIX}"
case ":${PATH}:" in
	*":${PREFIX}:"*)
		echo "✓ ${PREFIX} is on your PATH — try: git clone dosource://cloud/<repo>"
		;;
	*)
		echo "⚠ ${PREFIX} is NOT on your PATH. Add it, e.g.:"
		echo "    export PATH=\"${PREFIX}:\$PATH\""
		echo "  git needs git-remote-dosource on PATH to resolve dosource:// remotes."
		;;
esac
