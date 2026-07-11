#!/usr/bin/env bash
# Downloads latest release binary + web assets from GitHub and installs on server.
#
# Usage:
#   GITHUB_REPO="youruser/todo" ./scripts/deploy.sh
#
# Env vars:
#   GITHUB_REPO   required, "owner/repo" on GitHub
#   INSTALL_DIR   optional, default /opt/todo
#   SERVICE_NAME  optional, systemd service to restart after install, default todo

set -euo pipefail

GITHUB_REPO="${GITHUB_REPO:?set GITHUB_REPO=owner/repo}"
INSTALL_DIR="${INSTALL_DIR:-/opt/todo}"
SERVICE_NAME="${SERVICE_NAME:-todo}"
ASSET_NAME="todo-linux-amd64.tar.gz"

echo "==> fetching latest release info for ${GITHUB_REPO}"
API_URL="https://api.github.com/repos/${GITHUB_REPO}/releases/latest"

DOWNLOAD_URL=$(curl -fsSL "${API_URL}" | grep "browser_download_url.*${ASSET_NAME}" | cut -d '"' -f 4)

if [[ -z "${DOWNLOAD_URL}" ]]; then
  echo "error: could not find asset ${ASSET_NAME} in latest release" >&2
  exit 1
fi

echo "==> downloading ${DOWNLOAD_URL}"
TMP_DIR=$(mktemp -d)
trap 'rm -rf "${TMP_DIR}"' EXIT

curl -fsSL "${DOWNLOAD_URL}" -o "${TMP_DIR}/${ASSET_NAME}"

echo "==> extracting to ${TMP_DIR}/extracted"
mkdir -p "${TMP_DIR}/extracted"
tar -xzf "${TMP_DIR}/${ASSET_NAME}" -C "${TMP_DIR}/extracted"

echo "==> installing to ${INSTALL_DIR}"
sudo mkdir -p "${INSTALL_DIR}"
sudo cp "${TMP_DIR}/extracted/todo-server" "${INSTALL_DIR}/todo-server"
sudo chmod +x "${INSTALL_DIR}/todo-server"
sudo rm -rf "${INSTALL_DIR}/web"
sudo cp -r "${TMP_DIR}/extracted/web" "${INSTALL_DIR}/web"

if systemctl list-unit-files | grep -q "^${SERVICE_NAME}.service"; then
  echo "==> restarting systemd service ${SERVICE_NAME}"
  sudo systemctl restart "${SERVICE_NAME}"
else
  echo "==> systemd service ${SERVICE_NAME} not found, skipping restart"
  echo "    binary installed at ${INSTALL_DIR}/todo-server, run it manually or set up a service"
fi

echo "==> done"
