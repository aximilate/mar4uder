#!/bin/bash
set -e

echo "[*] Cleaning up old Python relay service if present..."
systemctl stop mar4uder-relay 2>/dev/null || true
systemctl disable mar4uder-relay 2>/dev/null || true
rm -f /etc/systemd/system/mar4uder-relay.service

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec bash "$SCRIPT_DIR/install.sh" "$@"

