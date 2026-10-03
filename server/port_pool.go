package main

import (
	"bufio"
	"math"
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

//go:embed static/*
var staticFS embed.FS

// SlotStreamState tracks on-demand WebRTC streaming status for a node slot
type SlotStreamState struct {
	mu            sync.Mutex
	activeViewers int
	streaming     bool
	lastActive    time.Time
	stopTimer     *time.Timer
	width         uint16
	height        uint16
	fps           uint16
	bitrateKb     uint16
	preset        uint8
}

// PortPoolManager manages dedicated sequential TCP ports for each node
// Slot N gets:
//   PTY Port = 9000 + N  (Raw interactive shell via nc/telnet)
//   VNC/WebRTC Port = 5900 + N  (HTTP WebRTC Stream in browser & RFC 6143 VNC)
type PortPoolManager struct {
	nm           *NodeManager
	udpGw        *UDPGateway
	rfbGw        *RFBGateway
	cfg          *Config
	ptyListeners map[int]net.Listener
	vncListeners map[int]net.Listener
	slotStates   map[int]*SlotStreamState
	mu           sync.Mutex
	stopChan     chan struct{}
}

// NewPortPoolManager initializes the sequential port manager
func NewPortPoolManager(nm *NodeManager, udpGw *UDPGateway, rfbGw *RFBGateway, cfg *Config) *PortPoolManager {
	return &PortPoolManager{
		nm:           nm,
		udpGw:        udpGw,
		rfbGw:        rfbGw,
		cfg:          cfg,
		ptyListeners: make(map[int]net.Listener),
		vncListeners: make(map[int]net.Listener),
		slotStates:   make(map[int]*SlotStreamState),
		stopChan:     make(chan struct{}),
	}
}

// Start begins periodic synchronization of dedicated node listeners & watchdog
func (ppm *PortPoolManager) Start() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	watchdogTicker := time.NewTicker(4 * time.Second)
	defer watchdogTicker.Stop()

	// Initial listener binding
	ppm.syncListeners()

	for {
		select {
		case <-ppm.stopChan:
			ppm.closeAll()
			return
		case <-ticker.C:
			ppm.syncListeners()
		case <-watchdogTicker.C:
			ppm.checkWatchdog()
		}
	}
}

// Stop terminates all port listeners
func (ppm *PortPoolManager) Stop() {
	close(ppm.stopChan)
}

func (ppm *PortPoolManager) closeAll() {
	ppm.mu.Lock()
	defer ppm.mu.Unlock()

	for port, l := range ppm.ptyListeners {
		_ = l.Close()
		delete(ppm.ptyListeners, port)
	}
	for port, l := range ppm.vncListeners {
		_ = l.Close()
		delete(ppm.vncListeners, port)
	}
}

func (ppm *PortPoolManager) getSlotState(slot int) *SlotStreamState {
	ppm.mu.Lock()
	defer ppm.mu.Unlock()
	st, exists := ppm.slotStates[slot]
	if !exists {
		st = &SlotStreamState{
			lastActive: time.Now(),
			fps:        30,
			bitrateKb:  2000,
			preset:     0,
		}
		ppm.slotStates[slot] = st
	}
	return st
}

// onDemandStart starts streaming when a viewer arrives
func (ppm *PortPoolManager) onDemandStart(target *Node, slot int) {
	if target == nil {
		return
	}
	target.mu.RLock()
	isOnline := time.Since(target.LastSeen) <= 15*time.Second
	target.mu.RUnlock()
	if !isOnline {
		return // Do not stream if node is offline!
	}

	st := ppm.getSlotState(slot)
	st.mu.Lock()
	defer st.mu.Unlock()

	if st.stopTimer != nil {
		st.stopTimer.Stop()
		st.stopTimer = nil
	}

	st.activeViewers++
	st.lastActive = time.Now()

	st.streaming = true
	serverHost := "104.143.206.163"
	streamName := fmt.Sprintf("desktop_slot_%d", slot)

	// Send UDP start burst (3 packets, 25ms apart) to guarantee delivery across packet loss
	width, height, fps, bitrateKb, preset := st.width, st.height, st.fps, st.bitrateKb, st.preset
	go func(tgt *Node, sHost, sName string, w, h, f, b uint16, p uint8) {
		for i := 0; i < 3; i++ {
			_ = ppm.udpGw.SendStreamStartExt(tgt, sHost, 8554, sName, w, h, f, b, p)
			time.Sleep(25 * time.Millisecond)
		}
	}(target, serverHost, streamName, width, height, fps, bitrateKb, preset)

	target.AddLog(fmt.Sprintf("On-Demand WebRTC stream active (viewer connected on port %d, stream %s, %d fps, %d kbps)", 5900+slot, streamName, st.fps, st.bitrateKb))
	log.Printf("[OnDemand] Slot #%d (%s): Active streaming (stream: %s, viewers: %d, fps: %d, br: %d kbps)", slot, target.ID, streamName, st.activeViewers, st.fps, st.bitrateKb)
}

// onDemandStop stops streaming when all viewers leave (after grace period)
func (ppm *PortPoolManager) onDemandStop(target *Node, slot int) {
	if target == nil {
		return
	}
	st := ppm.getSlotState(slot)
	st.mu.Lock()
	defer st.mu.Unlock()

	st.activeViewers--
	if st.activeViewers < 0 {
		st.activeViewers = 0
	}
	st.lastActive = time.Now()

	if st.activeViewers == 0 && st.streaming {
		if st.stopTimer != nil {
			st.stopTimer.Stop()
		}
		// 15-second grace period: if user refreshes page or renegotiates, ffmpeg won't stop and restart needlessly
		st.stopTimer = time.AfterFunc(15*time.Second, func() {
			st.mu.Lock()
			defer st.mu.Unlock()

			if st.activeViewers == 0 && st.streaming {
				st.streaming = false
				_ = ppm.udpGw.SendStreamStop(target)
				target.AddLog(fmt.Sprintf("On-Demand WebRTC stream stopped (0 viewers, saving CPU)"))
				log.Printf("[OnDemand] Slot #%d (%s): Stopped streaming (idle timeout, saving CPU)", slot, target.ID)
			}
		})
	}
}

// checkWatchdog checks go2rtc active consumers as a safety net to ensure zero idle CPU
func (ppm *PortPoolManager) checkWatchdog() {
	client := http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := client.Get("http://127.0.0.1:1984/api/streams")
	if err != nil {
		return
	}
	defer resp.Body.Close()

	var streams map[string]struct {
		Producers []struct {
			URL     string `json:"url"`
			BytesIn int64  `json:"bytes_in"`
		} `json:"producers"`
		Consumers []any `json:"consumers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&streams); err != nil {
		return
	}

	ppm.mu.Lock()
	defer ppm.mu.Unlock()

	for slot, st := range ppm.slotStates {
		target := ppm.nm.GetNodeBySlot(slot)
		if target == nil {
			continue
		}

		target.mu.RLock()
		isOnline := time.Since(target.LastSeen) <= 15*time.Second
		target.mu.RUnlock()

		streamKey := fmt.Sprintf("desktop_slot_%d", slot)
		streamObj, exists := streams[streamKey]
		numConsumers := 0
		hasActiveProducer := false
		if exists {
			numConsumers = len(streamObj.Consumers)
			for _, p := range streamObj.Producers {
				if p.URL != "" || p.BytesIn > 0 {
					hasActiveProducer = true
					break
				}
			}
		}
		// Fallback for slot 6 if viewer requested legacy "desktop"
		if slot == 6 && numConsumers == 0 {
			if ds, dExists := streams["desktop"]; dExists {
				numConsumers = len(ds.Consumers)
				for _, p := range ds.Producers {
					if p.URL != "" || p.BytesIn > 0 {
						hasActiveProducer = true
						break
					}
				}
			}
		}

		st.mu.Lock()
		// If web viewers or go2rtc consumers are present, keep stream alive
		if isOnline && (st.activeViewers > 0 || numConsumers > 0) {
			st.lastActive = time.Now()
			// If not streaming or producer hasn't connected yet, ensure start packet was delivered
			if !st.streaming || !hasActiveProducer {
				st.streaming = true
				streamName := fmt.Sprintf("desktop_slot_%d", slot)
				go func(tgt *Node, sName string, w, h, f, b uint16, p uint8) {
					_ = ppm.udpGw.SendStreamStartExt(tgt, "104.143.206.163", 8554, sName, w, h, f, b, p)
				}(target, streamName, st.width, st.height, st.fps, st.bitrateKb, st.preset)
			}
		}

		// Only collapse stream if BOTH web viewers and go2rtc consumers are 0 for > 15 seconds
		if st.streaming && st.activeViewers == 0 && numConsumers == 0 {
			if time.Since(st.lastActive) > 15*time.Second {
				st.streaming = false
				_ = ppm.udpGw.SendStreamStop(target)
				target.AddLog(fmt.Sprintf("Auto-standby: 0 viewers -> stream collapsed for Slot #%d (0%% CPU, 0 pkts)", slot))
				log.Printf("[AutoStandby] Slot #%d (%s): Stream collapsed (0 consumers, saving CPU)", slot, target.ID)
			}
		}
		st.mu.Unlock()
	}
}

func (ppm *PortPoolManager) syncListeners() {
	nodes := ppm.nm.ListNodes()

	ppm.mu.Lock()
	defer ppm.mu.Unlock()

	for _, n := range nodes {
		slot := n.Slot
		if slot <= 0 {
			continue
		}

		ptyPort := 9000 + slot
		vncPort := 5900 + slot

		// 1. Direct PTY Listener (Port 9000 + Slot)
		if _, exists := ppm.ptyListeners[ptyPort]; !exists {
			addr := fmt.Sprintf("0.0.0.0:%d", ptyPort)
			l, err := net.Listen("tcp", addr)
			if err != nil {
				log.Printf("[PortPool] Warning: Could not bind dedicated PTY port %d: %v", ptyPort, err)
			} else {
				ppm.ptyListeners[ptyPort] = l
				log.Printf("[PortPool] Dedicated PTY port :%d bound for Node [%s] (Slot #%d)", ptyPort, n.ID, slot)
				go ppm.acceptDirectPTY(l, slot)
			}
		}

		// 2. Direct VNC/WebRTC Multiplexed Listener (Port 5900 + Slot)
		if _, exists := ppm.vncListeners[vncPort]; !exists {
			addr := fmt.Sprintf("0.0.0.0:%d", vncPort)
			l, err := net.Listen("tcp", addr)
			if err != nil {
				log.Printf("[PortPool] Warning: Could not bind dedicated VNC/WebRTC port %d: %v", vncPort, err)
			} else {
				ppm.vncListeners[vncPort] = l
				log.Printf("[PortPool] Dedicated VNC/WebRTC port :%d bound for Node [%s] (Slot #%d)", vncPort, n.ID, slot)
				go ppm.acceptDirectVNC(l, slot)
			}
		}
	}
}

func (ppm *PortPoolManager) acceptDirectPTY(l net.Listener, slot int) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go ppm.handleDirectPTY(conn, slot)
	}
}

func (ppm *PortPoolManager) handleDirectPTY(conn net.Conn, slot int) {
	defer conn.Close()

	target := ppm.nm.GetNodeBySlot(slot)
	if target == nil {
		fmt.Fprintf(conn, "\r\n\x1b[31m[-] Node in Slot #%d is not registered.\x1b[0m\r\n", slot)
		return
	}

	target.mu.RLock()
	isOnline := time.Since(target.LastSeen) <= 15*time.Second
	nodeID := target.ID
	hostname := target.Hostname
	cols := target.Cols
	rows := target.Rows
	target.mu.RUnlock()

	if cols == 0 {
		cols = 120
	}
	if rows == 0 {
		rows = 35
	}

	if !isOnline {
		fmt.Fprintf(conn, "\r\n\x1b[31m[-] Node [%s] (#%d) is currently OFFLINE.\x1b[0m\r\n", nodeID, slot)
		return
	}

	// Admin Password Check
	if ppm.cfg != nil && ppm.cfg.GetAdminPassword() != "" {
		pass := ppm.cfg.GetAdminPassword()
		fmt.Fprintf(conn, "\r\n\x1b[1;37m+======================================================+\x1b[0m\r\n")
		fmt.Fprintf(conn, "\x1b[1;37m| // DEDSEC DIRECT PTY #%02d // AUTHENTICATION REQUIRED   |\x1b[0m\r\n", slot)
		fmt.Fprintf(conn, "\x1b[1;37m+======================================================+\x1b[0m\r\n")
		fmt.Fprintf(conn, "\x1b[1;33m[!] Enter Admin Password: \x1b[0m")

		reader := bufio.NewReader(conn)
		inputPass, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		cleanPass := strings.TrimSpace(cleanOperatorInput(inputPass))
		if cleanPass != pass && cleanPass != ppm.cfg.AuthToken {
			fmt.Fprintf(conn, "\r\n\x1b[1;31m[-] Access Denied: Incorrect Password.\x1b[0m\r\n\r\n")
			return
		}
		fmt.Fprintf(conn, "\r\n\x1b[1;32m[+] Access Granted. Attaching PTY...\x1b[0m\r\n\r\n")
	}

	// Open remote session with unique session ID
	sessID, err := ppm.udpGw.SendSessionOpen(target, cols, rows)
	if err != nil {
		fmt.Fprintf(conn, "\r\n\x1b[31m[-] Failed to open remote terminal session: %v\x1b[0m\r\n", err)
		return
	}

	subID := fmt.Sprintf("direct-pty-%d", sessID)
	ptyCh := target.AddPTYSubscriberForSession(subID, sessID)
	defer target.RemovePTYSubscriber(subID)
	defer func() {
		_ = ppm.udpGw.SendSessionCloseID(target, sessID)
	}()

	target.AddLog(fmt.Sprintf("Direct PTY connected from %s (Session #%d) via port :%d", conn.RemoteAddr().String(), sessID, 9000+slot))
	fmt.Fprintf(conn, "\x1b[1;32m[+] Connected directly to [%s] (%s) - Session #%d (Type exit to detach)\x1b[0m\r\n", nodeID, hostname, sessID)

	// Send enter to trigger shell prompt immediately
	time.Sleep(50 * time.Millisecond)
	_ = ppm.udpGw.SendPtyDataWithSession(target, sessID, []byte("\r"))

	stopCh := make(chan struct{})
	done := make(chan struct{})

	// Goroutine: Target PTY -> Client TCP Conn
	go func() {
		defer close(done)
		for {
			select {
			case <-stopCh:
				return
			case chunk, ok := <-ptyCh:
				if !ok {
					return
				}
				if _, err := conn.Write(chunk); err != nil {
					return
				}
			}
		}
	}()

	// Loop: Client TCP Conn -> Target PTY
	buf := make([]byte, 1024)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			break
		}
		if n > 0 {
			_ = ppm.udpGw.SendPtyDataWithSession(target, sessID, buf[:n])
		}
	}

	close(stopCh)
	_ = conn.Close()
	<-done
}

func (ppm *PortPoolManager) acceptDirectVNC(l net.Listener, slot int) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go ppm.handleDirectVNC(conn, slot)
	}
}

type prefixConn struct {
	net.Conn
	reader io.Reader
}

func (c *prefixConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

func (ppm *PortPoolManager) handleDirectVNC(conn net.Conn, slot int) {
	target := ppm.nm.GetNodeBySlot(slot)
	if target == nil {
		_ = conn.Close()
		return
	}

	// Sniff first 3 bytes to multiplex HTTP (Browser WebRTC) vs RFB (VNC Viewer)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	prefix := make([]byte, 3)
	n, err := io.ReadFull(conn, prefix)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		_ = conn.Close()
		return
	}

	combined := &prefixConn{
		Conn:   conn,
		reader: io.MultiReader(bytes.NewReader(prefix[:n]), conn),
	}

	pStr := strings.ToUpper(string(prefix[:n]))
	if pStr == "GET" || pStr == "POS" || pStr == "HEA" || pStr == "OPT" {
		// HTTP request: serve modern WebRTC HTML5 desktop
		ppm.serveSlotHTTP(combined, target, slot)
		return
	}

	// Standard RFB / VNC client
	ppm.rfbGw.HandleNodeClient(combined, target)
}

type singleConnListener struct {
	conn net.Conn
	done bool
	mu   sync.Mutex
}

func (s *singleConnListener) Accept() (net.Conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return nil, net.ErrClosed
	}
	s.done = true
	return s.conn, nil
}

func (s *singleConnListener) Close() error {
	return nil
}

func (s *singleConnListener) Addr() net.Addr {
	return s.conn.LocalAddr()
}

func (ppm *PortPoolManager) serveSlotHTTP(conn net.Conn, target *Node, slot int) {
	listener := &singleConnListener{conn: conn}
	handler := ppm.createSlotHTTPHandler(target, slot)
	server := &http.Server{
		Handler: handler,
	}
	_ = server.Serve(listener)
}


const dedsecLoginHTML = `<!DOCTYPE html>
<html lang="ru">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>DEDSEC // ACCESS CONTROL</title>
<style>
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body {
    background: #000000;
    color: #ffffff;
    font-family: 'JetBrains Mono', 'Fira Code', Consolas, monospace;
    display: flex;
    align-items: center;
    justify-content: center;
    height: 100vh;
    letter-spacing: -0.2px;
  }
  .login-card {
    border: 1px solid #333333;
    background: #080808;
    padding: 32px;
    width: min(380px, 92vw);
    text-align: center;
    box-shadow: 0 16px 40px rgba(0,0,0,0.9);
  }
  .brand { font-size: 14px; font-weight: 800; letter-spacing: 2px; margin-bottom: 6px; }
  .node-info { font-size: 11px; color: #888888; margin-bottom: 24px; border: 1px solid #222; padding: 4px; background: #000; }
  input {
    width: 100%;
    background: #000000;
    border: 1px solid #444444;
    color: #ffffff;
    padding: 8px 12px;
    font-size: 12px;
    font-family: inherit;
    outline: none;
    margin-bottom: 16px;
    text-align: center;
  }
  input:focus { border-color: #ffffff; }
  button {
    width: 100%;
    background: #ffffff;
    color: #000000;
    border: 1px solid #ffffff;
    padding: 8px 16px;
    font-size: 12px;
    font-family: inherit;
    font-weight: 700;
    cursor: pointer;
    text-transform: uppercase;
  }
  button:hover { background: #000000; color: #ffffff; }
  .err { color: #ff5555; font-size: 11px; margin-top: 12px; display: none; }
</style>
</head>
<body>
<div class="login-card">
  <div class="brand">// DEDSEC SECURITY //</div>
  <div class="node-info">TARGET: {{NODE_ID}} [SLOT #{{SLOT}}]</div>
  <form id="login-form" method="POST" action="/api/auth/login">
    <input type="password" name="password" id="pass-input" placeholder="ENTER ADMIN PASSWORD" autofocus required autocomplete="current-password" />
    <button type="submit">[ UNLOCK INTERFACE ]</button>
  </form>
  <div id="err-msg" class="err">[!] ACCESS DENIED: INVALID PASSWORD</div>
</div>
<script>
  if (new URLSearchParams(window.location.search).get('error') === '1') {
    document.getElementById('err-msg').style.display = 'block';
  }
</script>
</body>
</html>`

func (ppm *PortPoolManager) isAuthorized(r *http.Request) bool {
	if ppm.cfg == nil || ppm.cfg.GetAdminPassword() == "" {
		return true
	}
	pass := ppm.cfg.GetAdminPassword()
	if cookie, err := r.Cookie("m4r_auth"); err == nil && cookie.Value != "" {
		if cookie.Value == pass || cookie.Value == ppm.cfg.AuthToken {
			return true
		}
	}
	if qAuth := r.URL.Query().Get("auth"); qAuth != "" {
		if qAuth == pass || qAuth == ppm.cfg.AuthToken {
			return true
		}
	}
	if authHdr := r.Header.Get("Authorization"); authHdr != "" {
		token := strings.TrimPrefix(authHdr, "Bearer ")
		if token == pass || token == ppm.cfg.AuthToken {
			return true
		}
	}
	return false
}

func (ppm *PortPoolManager) createSlotHTTPHandler(target *Node, slot int) http.Handler {
	mux := http.NewServeMux()

	// Auth: Login endpoint
	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			_ = r.ParseForm()
			pass := r.FormValue("password")
			if pass == "" {
				var req struct {
					Password string `json:"password"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				pass = req.Password
			}
			if ppm.cfg != nil && ppm.cfg.CheckPassword(pass) {
				http.SetCookie(w, &http.Cookie{
					Name:     "m4r_auth",
					Value:    pass,
					Path:     "/",
					MaxAge:   86400 * 30,
					HttpOnly: false,
					SameSite: http.SameSiteLaxMode,
				})
				if r.Header.Get("Accept") == "application/json" {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
					return
				}
				http.Redirect(w, r, "/", http.StatusFound)
				return
			}
		}
		if r.Header.Get("Accept") == "application/json" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "message": "Invalid password"})
			return
		}
		http.Redirect(w, r, "/?error=1", http.StatusFound)
	})

	// Auth: Admin settings endpoint (view / change password)
	mux.HandleFunc("/api/admin/settings", func(w http.ResponseWriter, r *http.Request) {
		if !ppm.isAuthorized(r) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method == "POST" {
			var req struct {
				NewPassword string `json:"new_password"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if req.NewPassword != "" && ppm.cfg != nil {
				ppm.cfg.AdminPassword = req.NewPassword
				_ = SaveConfig("config.json", ppm.cfg)
				http.SetCookie(w, &http.Cookie{
					Name:     "m4r_auth",
					Value:    req.NewPassword,
					Path:     "/",
					MaxAge:   86400 * 30,
					HttpOnly: false,
				})
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "message": "Admin password updated successfully"})
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":         "ok",
			"admin_password": ppm.cfg.GetAdminPassword(),
			"auth_token":     ppm.cfg.AuthToken,
		})
	})

	// 1. Root and static web assets
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/" || path == "/index.html" {
			target.mu.RLock()
			nodeID := target.ID
			isOnline := time.Since(target.LastSeen) <= 15*time.Second
			target.mu.RUnlock()

			if !ppm.isAuthorized(r) {
				loginHTML := strings.ReplaceAll(dedsecLoginHTML, "{{NODE_ID}}", nodeID)
				loginHTML = strings.ReplaceAll(loginHTML, "{{SLOT}}", fmt.Sprintf("%d", slot))
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(loginHTML))
				return
			}

			data, err := staticFS.ReadFile("static/index.html")
			if err != nil {
				http.Error(w, "index.html not found", http.StatusInternalServerError)
				return
			}

			content := string(data)
			content = strings.ReplaceAll(content, "{{NODE_ID}}", nodeID)
			content = strings.ReplaceAll(content, "{{SLOT}}", fmt.Sprintf("%d", slot))
			if isOnline {
				content = strings.ReplaceAll(content, "{{IS_ONLINE}}", "true")
			} else {
				content = strings.ReplaceAll(content, "{{IS_ONLINE}}", "false")
			}

			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(content))
			return
		}

		cleanName := strings.TrimPrefix(path, "/")
		if data, err := staticFS.ReadFile("static/" + cleanName); err == nil {
			if strings.HasSuffix(cleanName, ".js") {
				w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
			} else if strings.HasSuffix(cleanName, ".css") {
				w.Header().Set("Content-Type", "text/css; charset=utf-8")
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}

		http.NotFound(w, r)
	})

	// 2. Reverse proxy /api/ to go2rtc (127.0.0.1:1984)
	go2rtcURL, _ := url.Parse("http://127.0.0.1:1984")
	proxy := httputil.NewSingleHostReverseProxy(go2rtcURL)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/ws") {
			ppm.onDemandStart(target, slot)
			defer ppm.onDemandStop(target, slot)
		}
		proxy.ServeHTTP(w, r)
	})

	// 3. /ws/input: Browser mouse & keyboard input WebSocket
	mux.HandleFunc("/ws/input", func(w http.ResponseWriter, r *http.Request) {
		ws, err := UpgradeWebSocket(w, r)
		if err != nil {
			log.Printf("[WebInput] Slot #%d WebSocket upgrade failed: %v", slot, err)
			return
		}
		defer ws.Close()

		ppm.onDemandStart(target, slot)
		defer ppm.onDemandStop(target, slot)

		var curBtnMask uint8 = 0

		for {
			op, payload, err := ws.ReadMessage()
			if err != nil {
				break
			}
			if op == wsOpText {
				var ev WebInputEvent
				if err := json.Unmarshal(payload, &ev); err == nil {
					liveTgt := ppm.nm.GetNodeBySlot(slot)
					if liveTgt == nil {
						liveTgt = target
					}
					inp := handleWebInputEvent(ev, &curBtnMask, liveTgt)
					if inp != nil && liveTgt != nil {
						_ = ppm.udpGw.SendVncInput(liveTgt, *inp)
					}
				}
			}
		}
	})

	// 4. /api/stream/config: Change quality or pause/resume stream on-demand
	mux.HandleFunc("/api/stream/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Scale   float64 `json:"scale"`
			Width   uint16  `json:"width"`
			Height  uint16  `json:"height"`
			Fps     uint16  `json:"fps"`
			Bitrate uint16  `json:"bitrate"`
			Preset  uint8   `json:"preset"`
			Action  string  `json:"action"` // "pause", "resume"
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		st := ppm.getSlotState(slot)
		st.mu.Lock()
		defer st.mu.Unlock()

		if req.Action == "pause" {
			st.streaming = false
			_ = ppm.udpGw.SendStreamStop(target)
			target.AddLog("Stream manually paused from web viewer (0% CPU)")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "paused"})
			return
		}

		if req.Action == "resume" {
			st.streaming = true
			st.lastActive = time.Now()
			streamName := fmt.Sprintf("desktop_slot_%d", slot)
			_ = ppm.udpGw.SendStreamStartExt(target, "104.143.206.163", 8554, streamName, st.width, st.height, st.fps, st.bitrateKb, st.preset)
			target.AddLog("Stream resumed from web viewer")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "resumed"})
			return
		}

		if req.Scale > 0 && req.Scale <= 1.0 {
			baseW := 1920
			baseH := 1080
			target.mu.RLock()
			if target.VncWidth > 0 && target.VncHeight > 0 {
				baseW = int(target.VncWidth)
				baseH = int(target.VncHeight)
			}
			target.mu.RUnlock()
			st.width = uint16(float64(baseW) * req.Scale)
			st.height = uint16(float64(baseH) * req.Scale)
		} else if req.Width > 0 && req.Height > 0 {
			st.width = req.Width
			st.height = req.Height
		}

		if req.Fps > 0 {
			st.fps = req.Fps
		}
		if req.Bitrate > 0 {
			st.bitrateKb = req.Bitrate
		}
		st.preset = req.Preset

		st.streaming = true
		st.lastActive = time.Now()
		streamName := fmt.Sprintf("desktop_slot_%d", slot)
		_ = ppm.udpGw.SendStreamStartExt(target, "104.143.206.163", 8554, streamName, st.width, st.height, st.fps, st.bitrateKb, st.preset)
		target.AddLog(fmt.Sprintf("Stream quality updated: %dx%d, %d FPS, %d kbps, preset %d", st.width, st.height, st.fps, st.bitrateKb, st.preset))

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"width":   st.width,
			"height":  st.height,
			"fps":     st.fps,
			"bitrate": st.bitrateKb,
			"preset":  st.preset,
		})
	})

	// 5. /api/stream/status: Live check if this node is online or offline
	mux.HandleFunc("/api/stream/status", func(w http.ResponseWriter, r *http.Request) {
		curTgt := ppm.nm.GetNodeBySlot(slot)
		if curTgt == nil {
			curTgt = target
		}
		if curTgt == nil {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"slot":      slot,
				"is_online": false,
				"width":     0,
				"height":    0,
			})
			return
		}
		curTgt.mu.RLock()
		isOnline := time.Since(curTgt.LastSeen) <= 15*time.Second
		nodeID := curTgt.ID
		hostname := curTgt.Hostname
		wPx := curTgt.VncWidth
		hPx := curTgt.VncHeight
		vncActive := curTgt.VncActive
		curTgt.mu.RUnlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"slot":       slot,
			"node_id":    nodeID,
			"hostname":   hostname,
			"is_online":  isOnline,
			"stream":     fmt.Sprintf("desktop_slot_%d", slot),
			"width":      wPx,
			"height":     hPx,
			"vnc_active": vncActive,
		})
	})

	// 5. /upload: Direct Web File Upload to remote node
	mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if err := r.ParseMultipartForm(128 << 20); err != nil {
			http.Error(w, "Failed to parse form: "+err.Error(), http.StatusBadRequest)
			return
		}

		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "Missing file: "+err.Error(), http.StatusBadRequest)
			return
		}
		defer file.Close()

		fileData, err := io.ReadAll(file)
		if err != nil {
			http.Error(w, "Failed reading file: "+err.Error(), http.StatusInternalServerError)
			return
		}

		targetPath := r.FormValue("path")
		if targetPath == "" {
			targetPath = fmt.Sprintf("/home/teacher/Рабочий стол/%s", header.Filename)
		}

		transferID := uint32(time.Now().UnixNano() & 0x7FFFFFFF)
		pushWait := target.AddFilePushWaiter(transferID)
		defer target.RemoveFilePushWaiter(transferID)

		_ = ppm.udpGw.SendFilePushStart(target, transferID, uint64(len(fileData)), 0755, targetPath)
		time.Sleep(10 * time.Millisecond)

		chunkSize := 1024
		for offset := 0; offset < len(fileData); offset += chunkSize {
			end := offset + chunkSize
			if end > len(fileData) {
				end = len(fileData)
			}
			_ = ppm.udpGw.SendFilePushChunk(target, transferID, uint64(offset), fileData[offset:end])
			time.Sleep(1 * time.Millisecond)
		}
		_ = ppm.udpGw.SendFilePushEnd(target, transferID)

		waitTimeout := 10*time.Second + time.Duration(len(fileData)/(50*1024))*time.Second
		timer := time.NewTimer(waitTimeout)
		defer timer.Stop()
		select {
		case status := <-pushWait:
			if status != 0 {
				http.Error(w, "Remote agent failed writing file", http.StatusInternalServerError)
				return
			}
		case <-timer.C:
		}

		// Ensure permissions for desktop user if file was written to /home/teacher
		if strings.HasPrefix(targetPath, "/home/teacher") {
			_ = ppm.udpGw.SendCmdExec(target, uint32(time.Now().UnixNano()&0x7FFFFFFF), 3,
				fmt.Sprintf("chown -R teacher:teacher %q 2>/dev/null", targetPath))
		}

		target.AddLog(fmt.Sprintf("Direct web upload completed: %s -> %s (%d bytes)", header.Filename, targetPath, len(fileData)))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":   "ok",
			"filename": header.Filename,
			"path":     targetPath,
			"size":     len(fileData),
		})
	})

	return mux
}

type WebInputEvent struct {
	Type   string  `json:"type"`
	X      int     `json:"x"`
	Y      int     `json:"y"`
	NormX  float64 `json:"normX"`
	NormY  float64 `json:"normY"`
	Button int     `json:"button"`
	DeltaY int     `json:"deltaY"`
	Key    string  `json:"key"`
	Code   string  `json:"code"`
}

func handleWebInputEvent(ev WebInputEvent, curMask *uint8, target *Node) *MsgVncInputPayload {
	var x, y uint16

	// Normalized 0..65535 coordinate space (accurate across all client devices and monitor resolutions)
	if ev.NormX > 0 || ev.NormY > 0 || (ev.X == 0 && ev.Y == 0) {
		nx := math.Max(0, math.Min(1.0, ev.NormX))
		ny := math.Max(0, math.Min(1.0, ev.NormY))
		x = uint16(math.Round(nx * 65535.0))
		y = uint16(math.Round(ny * 65535.0))
	} else {
		if ev.X > 65535 {
			ev.X = 65535
		}
		if ev.Y > 65535 {
			ev.Y = 65535
		}
		x = uint16(ev.X)
		y = uint16(ev.Y)
	}

	switch ev.Type {
	case "mousemove":
		return &MsgVncInputPayload{
			EventType:  0, // pointer
			ButtonMask: *curMask,
			X:          x,
			Y:          y,
		}
	case "mousedown":
		var bit uint8 = 1 // Left (button 0)
		if ev.Button == 1 {
			bit = 2 // Middle (button 1)
		} else if ev.Button == 2 {
			bit = 4 // Right (button 2)
		}
		*curMask |= bit
		return &MsgVncInputPayload{
			EventType:  0,
			ButtonMask: *curMask,
			X:          x,
			Y:          y,
		}
	case "mouseup":
		var bit uint8 = 1
		if ev.Button == 1 {
			bit = 2
		} else if ev.Button == 2 {
			bit = 4
		}
		*curMask &= ^bit
		return &MsgVncInputPayload{
			EventType:  0,
			ButtonMask: *curMask,
			X:          x,
			Y:          y,
		}
	case "wheel":
		var wBit uint8 = 8 // Wheel Up
		if ev.DeltaY > 0 {
			wBit = 16 // Wheel Down
		}
		return &MsgVncInputPayload{
			EventType:  0,
			ButtonMask: *curMask | wBit,
			X:          x,
			Y:          y,
		}
	case "keydown":
		keysym := keyToX11Keysym(ev.Key, ev.Code)
		return &MsgVncInputPayload{
			EventType: 1, // key
			DownFlag:  1,
			KeySym:    keysym,
		}
	case "keyup":
		keysym := keyToX11Keysym(ev.Key, ev.Code)
		return &MsgVncInputPayload{
			EventType: 1, // key
			DownFlag:  0,
			KeySym:    keysym,
		}
	}
	return nil
}

func keyToX11Keysym(key string, code string) uint32 {
	switch key {
	case "Enter":
		return 0xFF0D
	case "Backspace":
		return 0xFF08
	case "Tab":
		return 0xFF09
	case "Escape":
		return 0xFF1B
	case "Delete":
		return 0xFFFF
	case "Insert":
		return 0xFF63
	case "Space", " ":
		return 0x0020
	case "ArrowLeft":
		return 0xFF51
	case "ArrowUp":
		return 0xFF52
	case "ArrowRight":
		return 0xFF53
	case "ArrowDown":
		return 0xFF54
	case "PageUp":
		return 0xFF55
	case "PageDown":
		return 0xFF56
	case "Home":
		return 0xFF50
	case "End":
		return 0xFF57
	case "Shift", "ShiftLeft", "ShiftRight", "Shift_L", "Shift_R":
		return 0xFFE1
	case "Control", "ControlLeft", "ControlRight", "Control_L", "Control_R", "Ctrl":
		return 0xFFE3
	case "Alt", "AltLeft", "AltRight", "Alt_L", "Alt_R":
		return 0xFFE9
	case "Meta", "MetaLeft", "MetaRight", "Super", "Super_L", "Super_R", "OS", "Win":
		return 0xFFEB
	case "CapsLock":
		return 0xFFE5
	case "PrintScreen":
		return 0xFF61
	case "ScrollLock":
		return 0xFF14
	case "Pause":
		return 0xFF13
	case "NumLock":
		return 0xFF7F
	case "ContextMenu":
		return 0xFF67
	case "F1":
		return 0xFFBE
	case "F2":
		return 0xFFBF
	case "F3":
		return 0xFFC0
	case "F4":
		return 0xFFC1
	case "F5":
		return 0xFFC2
	case "F6":
		return 0xFFC3
	case "F7":
		return 0xFFC4
	case "F8":
		return 0xFFC5
	case "F9":
		return 0xFFC6
	case "F10":
		return 0xFFC7
	case "F11":
		return 0xFFC8
	case "F12":
		return 0xFFC9
	}

	// Physical key code fallback for standard letter/digit keys (works across all keyboard layouts)
	if strings.HasPrefix(code, "Key") && len(code) == 4 {
		return uint32(strings.ToLower(code[3:])[0])
	}
	if strings.HasPrefix(code, "Digit") && len(code) == 6 {
		return uint32(code[5])
	}

	// Check Unicode codepoint for single characters
	runes := []rune(key)
	if len(runes) == 1 {
		r := runes[0]
		if r < 128 {
			return uint32(r)
		}
		// X11 Unicode Keysym: 0x01000000 | codepoint
		return 0x01000000 | uint32(r)
	}

	return 0
}
