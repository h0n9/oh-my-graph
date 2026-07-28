#!/bin/sh
set -e

REPO="h0n9/oh-my-graph"
BINARY="oh-my-graph"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  darwin|linux) ;;
  *)
    echo "error: unsupported OS: $os" >&2
    exit 1
    ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *)
    echo "error: unsupported architecture: $arch" >&2
    exit 1
    ;;
esac

version="${VERSION:-}"
if [ -z "$version" ]; then
  # Resolve via the plain github.com redirect rather than api.github.com/releases/latest —
  # the API is rate-limited per source IP (60/hr unauthenticated), which shared egress IPs
  # (e.g. on Sprites) can exhaust quickly; this redirect isn't subject to that limit.
  version=$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/${REPO}/releases/latest" | sed -E 's#.*/tag/##')
fi
if [ -z "$version" ]; then
  echo "error: could not determine latest version (set VERSION=vX.Y.Z to pin one)" >&2
  exit 1
fi

install_dir="${INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$install_dir"

url="https://github.com/${REPO}/releases/download/${version}/${BINARY}_${os}_${arch}.tar.gz"
echo "Installing ${BINARY} ${version} (${os}/${arch}) to ${install_dir}..."

tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT

curl -fsSL "$url" | tar xz -C "$tmp_dir"
mv "$tmp_dir/${BINARY}" "$install_dir/${BINARY}"
chmod +x "$install_dir/${BINARY}"

echo "${BINARY} ${version} installed to ${install_dir}/${BINARY}"

case ":$PATH:" in
  *":$install_dir:"*) ;;
  *)
    echo
    echo "warning: ${install_dir} is not on your PATH."
    echo "  add this to your shell profile:"
    echo "    export PATH=\"${install_dir}:\$PATH\""
    ;;
esac

echo
echo "Run '${BINARY}' to start the server (listens on :7780, data at ~/.oh-my-graph)."
echo "Point your MCP client at http://localhost:7780/mcp"
