#!/bin/sh
#
# install.sh installs a Hive release.
#
# It downloads the tarball for this platform, verifies its SHA-256 against the
# release's checksums.txt, and installs hive together with its plugin binaries
# side by side — which is where the daemon looks for them.
#
#   curl -fsSL https://github.com/thupham/hive/releases/latest/download/install.sh | sh
#   ./install.sh --version 0.1.0 --bin-dir "$HOME/.local/bin"
#
# The URL above is the copy published with the release, not the one on a branch:
# a branch is mutable, so what you pipe to sh would not be what any release
# shipped.
#
# Environment:
#   HIVE_VERSION   the release to install, without the leading v (default: latest)
#   HIVE_BIN_DIR   where to install the binaries (default: $HOME/.local/bin)
#
set -eu

REPO="thupham/hive"
VERSION="${HIVE_VERSION:-}"
BIN_DIR="${HIVE_BIN_DIR:-${HOME:-}/.local/bin}"

usage() {
	cat <<'EOF'
Install a Hive release.

Usage:
  install.sh [--version <x.y.z>] [--bin-dir <dir>]

Options:
  --version <x.y.z>  the release to install (default: the latest)
  --bin-dir <dir>    where to install the binaries (default: $HOME/.local/bin)
  -h, --help         print this and exit

Environment:
  HIVE_VERSION       same as --version
  HIVE_BIN_DIR       same as --bin-dir
EOF
}

die() {
	printf 'install.sh: %s\n' "$*" >&2
	exit 1
}

note() {
	printf 'install.sh: %s\n' "$*" >&2
}

while [ $# -gt 0 ]; do
	case "$1" in
	--version)
		[ $# -ge 2 ] || die "--version needs a value"
		VERSION="$2"
		shift 2
		;;
	--bin-dir)
		[ $# -ge 2 ] || die "--bin-dir needs a value"
		BIN_DIR="$2"
		shift 2
		;;
	-h | --help)
		usage
		exit 0
		;;
	*) die "unknown argument: $1 (try --help)" ;;
	esac
done

[ -n "${HOME:-}" ] || die "HOME is not set, so there is nowhere to install; pass --bin-dir"

# The tag carries a "v" and the asset name does not, so it is stripped once, here,
# whichever way the version arrived — the flag and the environment variable behave
# the same.
VERSION="${VERSION#v}"

# The downloader and the checksum tool are the only two external programs this
# script needs, and both have a portable spelling on macOS and Linux.
HAVE_CURL=""
if command -v curl >/dev/null 2>&1; then
	HAVE_CURL=1
	download() { curl -fsSL "$1" -o "$2"; }
	fetch() { curl -fsSL "$1"; }
elif command -v wget >/dev/null 2>&1; then
	download() { wget -qO "$2" "$1"; }
	fetch() { wget -qO- "$1"; }
else
	die "curl or wget is required"
fi

if command -v sha256sum >/dev/null 2>&1; then
	sha256_of() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
	sha256_of() { shasum -a 256 "$1" | awk '{print $1}'; }
else
	die "sha256sum or shasum is required to verify the download"
fi

install_bin() {
	target_dir=$(dirname "$2")
	target_name=$(basename "$2")

	# The copy is staged beside its target, not in $tmp, because a rename is only
	# atomic within one filesystem. Staging and renaming is what lets an upgrade
	# replace a binary the daemon is running: writing over it in place does not
	# work, and Linux refuses it with "Text file busy".
	staged="${target_dir}/.${target_name}.new.$$"
	cp "$1" "$staged"
	chmod 0755 "$staged"
	mv -f "$staged" "$2"
	staged=""
}

# Which platform this is. The names are Go's, because that is what the release
# artifacts are named after.
os=$(uname -s)
case "$os" in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) die "$os is not supported; Hive releases cover linux and darwin" ;;
esac

arch=$(uname -m)
case "$arch" in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*) die "$arch is not supported; Hive releases cover amd64 and arm64" ;;
esac

# darwin/amd64 has no published artifact: GitHub retired the amd64 macOS runner,
# so there is no native host to build it on. Say so plainly rather than
# reporting a missing download.
if [ "$os" = darwin ] && [ "$arch" = amd64 ]; then
	die "darwin/amd64 is not published; build from source on this machine with 'make build'"
fi

# latest_version resolves the newest release tag, without its leading "v".
#
# The redirect on /releases/latest names the tag, which is used when curl is
# available. The API is rate-limited per address, so a shared network could fail
# on the one request this script makes; it stays as the portable fallback,
# because wget prints a redirect in a shape that varies between builds.
latest_version() {
	if [ -n "$HAVE_CURL" ]; then
		url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' \
			"https://github.com/${REPO}/releases/latest" 2>/dev/null) || url=""
		tag="${url##*/}"
		if [ -n "$tag" ] && [ "$tag" != latest ]; then
			printf '%s\n' "${tag#v}"
			return
		fi
	fi

	fetch "https://api.github.com/repos/${REPO}/releases/latest" |
		sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"v\{0,1\}\([^"]*\)".*/\1/p' | head -n 1
}

if [ -z "$VERSION" ]; then
	# The asset name carries the version, so the version has to be resolved
	# before anything can be downloaded.
	VERSION=$(latest_version)
	[ -n "$VERSION" ] || die "could not resolve the latest release; pass --version"
fi

asset="hive_${VERSION}_${os}_${arch}.tar.gz"
base="https://github.com/${REPO}/releases/download/v${VERSION}"

tmp=$(mktemp -d)

# A staged binary is removed too: an install that stops between staging and
# renaming must not leave a hidden half-copy beside the real one.
staged=""
cleanup() {
	if [ -n "$staged" ]; then
		rm -f "$staged"
	fi
	rm -rf "$tmp"
}
trap cleanup EXIT INT TERM

note "downloading $asset"
download "${base}/${asset}" "${tmp}/${asset}" ||
	die "could not download ${asset}; does release v${VERSION} have a build for ${os}/${arch}?"
download "${base}/checksums.txt" "${tmp}/checksums.txt" ||
	die "could not download checksums.txt for v${VERSION}"

# The checksum is verified before the archive is opened. An archive that is
# never extracted cannot do anything, whatever is in it.
#
# The name is compared without the "./" a shell glob may have left on it, without
# the "*" a checksum tool may prefix in binary mode, and without a carriage return
# a file written on Windows would leave on it.
expected=$(awk -v a="$asset" '
	{
		n = $2
		sub(/\r$/, "", n)
		sub(/^\*/, "", n)
		sub(/^\.\//, "", n)
		if (n == a) { print $1; exit }
	}' "${tmp}/checksums.txt")
[ -n "$expected" ] || die "checksums.txt has no entry for ${asset}"
actual=$(sha256_of "${tmp}/${asset}")
[ "$expected" = "$actual" ] || die "checksum mismatch for ${asset}
  expected ${expected}
  actual   ${actual}
Refusing to install a download that does not match its checksum."

mkdir -p "${tmp}/extract"
tar -xzf "${tmp}/${asset}" -C "${tmp}/extract"

mkdir -p "$BIN_DIR"

# The plugin binaries are installed next to hive on purpose: the daemon resolves
# them from its own directory, and hive-plugin-slack is a transport that has to
# be found the same way.
for name in hive hive-plugin-acp hive-plugin-slack; do
	[ -f "${tmp}/extract/${name}" ] || die "${asset} does not contain ${name}"
	[ ! -L "${tmp}/extract/${name}" ] || die "${asset} has a symlink where ${name} should be"
	install_bin "${tmp}/extract/${name}" "${BIN_DIR}/${name}"
done

printf 'installed hive %s to %s\n' "$VERSION" "$BIN_DIR"

case ":${PATH}:" in
*":${BIN_DIR}:"*) ;;
*) note "add ${BIN_DIR} to PATH: export PATH=\"${BIN_DIR}:\$PATH\"" ;;
esac

# A running daemon keeps the binary it was started from: replacing the file does
# not replace the process. An upgrade is therefore finished with a restart, and
# saying so here is the difference between "the new build works" and "the upgrade
# did nothing".
service_installed() {
	[ -f "${HOME}/Library/LaunchAgents/ai.hive.daemon.plist" ] ||
		[ -f "${HOME}/.config/systemd/user/ai.hive.daemon.service" ]
}

printf '\nNext:\n'
if [ -f "${HOME}/.hive/config.toml" ]; then
	if service_installed; then
		printf '  hive service restart   # run the new build; the daemon still has the old one\n'
	else
		printf '  hive serve             # run the coordinator, a node, and your agents\n'
	fi
else
	printf '  hive init              # find your ACP agents and write ~/.hive/config.toml\n'
	printf '  hive serve             # run the coordinator, a node, and your agents\n'
fi
