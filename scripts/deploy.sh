#!/usr/bin/env bash
# Downloads the latest standalone Linux executable from GitHub and installs for the current user.
#
# Usage:
#   GITHUB_REPO="youruser/todo" ./scripts/deploy.sh
#
# Env vars:
#   GITHUB_REPO   required, "owner/repo" on GitHub
#   INSTALL_DIR   optional, default $HOME/.local/share/todo
#   SERVICE_NAME  optional, systemd --user service to restart after install, default todo

set -euo pipefail

GITHUB_REPO="${GITHUB_REPO:?set GITHUB_REPO=owner/repo}"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/share/todo}"
SERVICE_NAME="${SERVICE_NAME:-todo}"
case "$(uname -m)" in
  x86_64|amd64) RELEASE_ARCH="amd64" ;;
  aarch64|arm64) RELEASE_ARCH="arm64" ;;
  *) echo "error: unsupported architecture $(uname -m); releases are available for amd64 and arm64" >&2; exit 1 ;;
esac
ASSET_NAME="todo-server-linux-${RELEASE_ARCH}"

echo "==> fetching latest release info for ${GITHUB_REPO}"
API_URL="https://api.github.com/repos/${GITHUB_REPO}/releases/latest"

if ! DOWNLOAD_URL=$(curl -fsSL "${API_URL}" | grep "browser_download_url.*${ASSET_NAME}" | cut -d '"' -f 4); then
  DOWNLOAD_URL=""
fi

if [[ -z "${DOWNLOAD_URL}" ]]; then
  echo "error: could not find asset ${ASSET_NAME} in latest release" >&2
  exit 1
fi

echo "==> downloading ${ASSET_NAME}"
TMP_DIR=$(mktemp -d)
NEW_BINARY=""
cleanup() {
  rm -rf "${TMP_DIR}"
  if [[ -n "${NEW_BINARY}" ]]; then
    rm -f "${NEW_BINARY}"
  fi
}
trap cleanup EXIT

curl -fsSL "${DOWNLOAD_URL}" -o "${TMP_DIR}/${ASSET_NAME}"

echo "==> installing to ${INSTALL_DIR}"
mkdir -p "${INSTALL_DIR}"
NEW_BINARY="${INSTALL_DIR}/.todo-server.$$"
cp "${TMP_DIR}/${ASSET_NAME}" "${NEW_BINARY}"
chmod +x "${NEW_BINARY}"
mv -f "${NEW_BINARY}" "${INSTALL_DIR}/todo-server"
NEW_BINARY=""

if systemctl --user list-unit-files 2>/dev/null | grep -q "^${SERVICE_NAME}.service"; then
  echo "==> restarting systemd --user service ${SERVICE_NAME}"
  systemctl --user restart "${SERVICE_NAME}"
else
  echo "==> systemd --user service ${SERVICE_NAME} not found, skipping restart"
  echo "    binary installed at ${INSTALL_DIR}/todo-server, run it manually or set up a user service"
  echo "    (see scripts/todo.service, install to ~/.config/systemd/user/)"
fi

echo "==> done"
