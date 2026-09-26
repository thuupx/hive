#!/bin/sh
#
# install.sh installs a Hive release.
#
# It downloads the tarball for this platform, verifies its SHA-256 against the
# release's checksums.txt, and installs hive together with its plugin binaries
# side by side — which is where the daemon looks for them.
#
#   curl -fsSL https://raw.githubusercontent.com/thupham/hive/main/scripts/install.sh | sh
#   ./install.sh --version 0.1.0 --bin-dir "$HOME/.local/bin"
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
		VERSION="${2#v}"
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

# The downloader and the checksum tool are the only two external programs this
# script needs, and both have a portable spelling on macOS and Linux.
if command -v curl >/dev/null 2>&1; then
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
	if command -v install >/dev/null 2>&1; then
		install -m 0755 "$1" "$2"
	else
		cp "$1" "$2" && chmod 0755 "$2"
	fi
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

if [ -z "$VERSION" ]; then
	# The asset name carries the version, so the version has to be resolved
	# before anything can be downloaded.
	VERSION=$(fetch "https://api.github.com/repos/${REPO}/releases/latest" |
		sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"v\{0,1\}\([^"]*\)".*/\1/p' | head -n 1)
	[ -n "$VERSION" ] || die "could not resolve the latest release; pass --version"
fi

asset="hive_${VERSION}_${os}_${arch}.tar.gz"
base="https://github.com/${REPO}/releases/download/v${VERSION}"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

note "downloading $asset"
download "${base}/${asset}" "${tmp}/${asset}" ||
	die "could not download ${asset}; does release v${VERSION} have a build for ${os}/${arch}?"
download "${base}/checksums.txt" "${tmp}/checksums.txt" ||
	die "could not download checksums.txt for v${VERSION}"

# The checksum is verified before the archive is opened. An archive that is
# never extracted cannot do anything, whatever is in it.
#
# The name is compared without the "./" a shell glob may have left on it, and
# without the "*" a checksum tool may prefix in binary mode.
expected=$(awk -v a="$asset" '
	{
		n = $2
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
	install_bin "${tmp}/extract/${name}" "${BIN_DIR}/${name}"
done

printf 'installed hive %s to %s\n' "$VERSION" "$BIN_DIR"

case ":${PATH}:" in
*":${BIN_DIR}:"*) ;;
*) note "add ${BIN_DIR} to PATH: export PATH=\"${BIN_DIR}:\$PATH\"" ;;
esac

printf '\nNext:\n  hive init     # find your ACP agents and write ~/.hive/config.toml\n  hive serve    # run the coordinator, a node, and your agents\n'
