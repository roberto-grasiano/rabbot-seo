#!/bin/sh
# Rabbot-SEO installer — downloads the right release binary for your OS/arch,
# verifies its SHA256 against the published checksums.txt, and installs it.
#
#   curl -fsSL https://raw.githubusercontent.com/roberto-grasiano/rabbot-seo/main/install.sh | sh
#
# Options (env vars):
#   RABBOT_VERSION=v0.1.0   install a specific tag (default: latest release)
#   RABBOT_INSTALL_DIR=...   install location (default: /usr/local/bin, else ~/.local/bin)
#
# Windows: use Scoop instead (see the README). This script supports Linux + macOS.
set -eu

REPO="roberto-grasiano/rabbot-seo"
PROJECT="rabbot-seo"
BIN="rabbot"

say()  { printf '%s\n' "$*"; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

have curl || have wget || die "need curl or wget"
have tar || die "need tar"

fetch() { # fetch <url> <dest>
  if have curl; then curl -fsSL "$1" -o "$2"
  else wget -qO "$2" "$1"; fi
}
fetch_stdout() { # fetch <url> -> stdout
  if have curl; then curl -fsSL "$1"
  else wget -qO- "$1"; fi
}

# --- detect platform --------------------------------------------------------
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$os" in
  linux) os=linux ;;
  darwin) os=darwin ;;
  *) die "unsupported OS '$os' (Windows: install with Scoop — see the README)" ;;
esac
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) die "unsupported architecture '$arch'" ;;
esac

# --- resolve version --------------------------------------------------------
tag="${RABBOT_VERSION:-}"
if [ -z "$tag" ]; then
  say "Resolving the latest release…"
  tag=$(fetch_stdout "https://api.github.com/repos/${REPO}/releases/latest" \
        | grep '"tag_name"' | head -1 \
        | sed -E 's/.*"tag_name"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/')
fi
[ -n "$tag" ] || die "could not resolve a release tag — set RABBOT_VERSION=vX.Y.Z (and make sure a non-draft release exists)"
ver="${tag#v}"

archive="${PROJECT}_${ver}_${os}_${arch}.tar.gz"
base="https://github.com/${REPO}/releases/download/${tag}"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

say "Downloading ${archive} (${tag})…"
fetch "${base}/${archive}" "${tmp}/${archive}"   || die "download failed: ${base}/${archive}"
fetch "${base}/checksums.txt" "${tmp}/checksums.txt" || die "could not fetch checksums.txt"

# --- verify checksum --------------------------------------------------------
say "Verifying SHA256…"
(
  cd "$tmp"
  line=$(grep " ${archive}\$" checksums.txt) || die "no checksum entry for ${archive}"
  if have sha256sum; then printf '%s\n' "$line" | sha256sum -c -
  elif have shasum; then printf '%s\n' "$line" | shasum -a 256 -c -
  else die "no sha256 tool (need sha256sum or shasum)"; fi
) || die "checksum verification FAILED — aborting"

# --- install ----------------------------------------------------------------
tar -xzf "${tmp}/${archive}" -C "$tmp"
[ -f "${tmp}/${BIN}" ] || die "archive did not contain '${BIN}'"

dir="${RABBOT_INSTALL_DIR:-}"
if [ -z "$dir" ]; then
  if [ -w /usr/local/bin ] 2>/dev/null; then dir="/usr/local/bin"; else dir="${HOME}/.local/bin"; fi
fi
mkdir -p "$dir"
install -m 0755 "${tmp}/${BIN}" "${dir}/${BIN}" 2>/dev/null \
  || { cp "${tmp}/${BIN}" "${dir}/${BIN}" && chmod 0755 "${dir}/${BIN}"; }

say ""
say "Installed ${BIN} ${tag} to ${dir}/${BIN}"
"${dir}/${BIN}" version 2>/dev/null || true
case ":${PATH}:" in
  *":${dir}:"*) : ;;
  *) say ""; say "Note: ${dir} is not on your PATH — add it, e.g.:"; say "  echo 'export PATH=\"${dir}:\$PATH\"' >> ~/.profile" ;;
esac
say ""
say "Next: run 'rabbot init' to set up your first site."
