package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// PersistentNodeMeta stores persistent metadata like notes and preferred VNC engine
type PersistentNodeMeta struct {
	Slot      int    `json:"slot"`
	Note      string `json:"note"`
	VncEngine string `json:"vnc_engine"`
	VncPort   uint16 `json:"vnc_port"`
	VncScale  int    `json:"vnc_scale"` // 1, 2, 3, 4, 0=auto
	VncBpp    int    `json:"vnc_bpp"`   // 8, 16, 32, 0=default(16)
}

// PTYSubscriber tracks a client terminal subscriber and optional session filtering
type PTYSubscriber struct {
	Ch        chan []byte
	SessionID uint32
}

// Node represents an active connected agent
type Node struct {
	Slot            int          `json:"slot"`
	ID              string       `json:"id"`
	Hostname        string       `json:"hostname"`
	OSInfo          string       `json:"os_info"`
	RemoteAddr      string       `json:"remote_addr"`
	LastSeen        time.Time    `json:"last_seen"`
	ActiveSessionID uint32       `json:"active_session_id"`
	Cols            uint16       `json:"cols"`
	Rows            uint16       `json:"rows"`
	HasRoot         bool         `json:"has_root"`
	VncActive       bool         `json:"vnc_active"`
	VncWidth        uint16       `json:"vnc_width"`
	VncHeight       uint16       `json:"vnc_height"`
	VncBpp          uint8        `json:"vnc_bpp"`
	Note            string       `json:"note"`       // Custom user label (e.g. "Кабинет 32")
	VncEngine       string       `json:"vnc_engine"` // "custom", "krfb", "x11vnc"
	VncPort         uint16       `json:"vnc_port"`   // e.g. 5900, 5901
	VncScale        int          `json:"vnc_scale"`  // 1, 2, 3, 4, 0=auto
	VncTargetBpp    int          `json:"vnc_target_bpp"` // 8, 16, 32
	ActiveVncClients int         `json:"active_vnc_clients"`
	DebugLogs       []string     `json:"debug_logs"` // Recent events and error logs
	udpAddr          *net.UDPAddr
	seqCounter       uint32
	mu               sync.RWMutex
	ptySubscribers   map[string]*PTYSubscriber
	vncSubscribers   map[string]chan []byte
	cmdChannels      map[uint32]chan MsgCmdOutputPayload
	fsListChannels   map[uint32]chan []FsEntry
	filePushChannels map[uint32]chan uint8
	filePullChannels map[uint32]chan MsgFileChunkPayload
	FullFb           []byte
	FullFbMu         sync.RWMutex
}

// NodeSummary is the JSON structure returned by the REST API and TUI
type NodeSummary struct {
	Slot            int    `json:"slot"`
	ID              string `json:"id"`
	Hostname        string `json:"hostname"`
	OSInfo          string `json:"os_info"`
	RemoteAddr      string `json:"remote_addr"`
	LastSeenAgoSec  int64  `json:"last_seen_ago_sec"`
	IsOnline        bool   `json:"is_online"`
	ActiveSessionID uint32 `json:"active_session_id"`
	Cols            uint16 `json:"cols"`
	Rows            uint16 `json:"rows"`
	HasRoot         bool   `json:"has_root"`
	VncActive       bool   `json:"vnc_active"`
	Note            string `json:"note"`
	VncEngine       string `json:"vnc_engine"`
	VncPort         uint16 `json:"vnc_port"`
	VncScale        int    `json:"vnc_scale"`
	VncTargetBpp    int    `json:"vnc_target_bpp"`
	ActiveVncClients int   `json:"active_vnc_clients"`
}

// NodeManager coordinates all registered agents
type NodeManager struct {
	mu         sync.RWMutex
	nodes      map[string]*Node
	metaPath   string
	metaMap    map[string]PersistentNodeMeta
}

// NewNodeManager initializes a new NodeManager with persistent metadata
func NewNodeManager(metaPath string) *NodeManager {
	if metaPath == "" {
		metaPath = "nodes_config.json"
	}

	nm := &NodeManager{
		nodes:    make(map[string]*Node),
		metaPath: metaPath,
		metaMap:  make(map[string]PersistentNodeMeta),
	}
	nm.loadMeta()
	return nm
}

func (nm *NodeManager) loadMeta() {
	data, err := os.ReadFile(nm.metaPath)
	if err == nil {
		_ = json.Unmarshal(data, &nm.metaMap)
		for id, meta := range nm.metaMap {
			if meta.Slot <= 0 {
				continue
			}
			engine := meta.VncEngine
			if engine == "" {
				engine = "custom"
			}
			port := meta.VncPort
			if port == 0 {
				port = 5900
			}
			bpp := meta.VncBpp
			if bpp == 0 {
				bpp = 16
			}
			nm.nodes[id] = &Node{
				Slot:             meta.Slot,
				ID:               id,
				Hostname:         id,
				OSInfo:           "Linux",
				RemoteAddr:       "offline",
				LastSeen:         time.Time{}, // past timestamp -> marked OFFLINE
				Cols:             80,
				Rows:             24,
				Note:             meta.Note,
				VncEngine:        engine,
				VncPort:          port,
				VncScale:         meta.VncScale,
				VncTargetBpp:     bpp,
				DebugLogs:        []string{fmt.Sprintf("[%s] Restored from persistent config (Slot #%d)", time.Now().Format("15:04:05"), meta.Slot)},
				seqCounter:       1,
				ptySubscribers:   make(map[string]*PTYSubscriber),
				vncSubscribers:   make(map[string]chan []byte),
				cmdChannels:      make(map[uint32]chan MsgCmdOutputPayload),
				fsListChannels:   make(map[uint32]chan []FsEntry),
				filePushChannels: make(map[uint32]chan uint8),
				filePullChannels: make(map[uint32]chan MsgFileChunkPayload),
			}
		}
	}
}

func (nm *NodeManager) saveMeta() {
	data, err := json.MarshalIndent(nm.metaMap, "", "  ")
	if err == nil {
		_ = os.WriteFile(nm.metaPath, data, 0644)
	}
}

// RegisterOrUpdate records node heartbeat / registration
func (nm *NodeManager) RegisterOrUpdate(id, hostname, osInfo string, addr *net.UDPAddr, isRoot bool) *Node {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	node, exists := nm.nodes[id]
	if !exists {
		// Restore persistent note & engine & slot & VNC quality if previously saved
		note := ""
		vncEngine := "custom"
		vncPort := uint16(5900)
		vncScale := 0
		vncBpp := 16
		slot := 0
		if meta, ok := nm.metaMap[id]; ok {
			note = meta.Note
			if meta.VncEngine != "" {
				vncEngine = meta.VncEngine
			}
			if meta.VncPort > 0 {
				vncPort = meta.VncPort
			}
			if meta.VncScale > 0 {
				vncScale = meta.VncScale
			}
			if meta.VncBpp > 0 {
				vncBpp = meta.VncBpp
			}
			slot = meta.Slot
		}

		if slot <= 0 {
			// Find lowest available slot >= 1
			usedSlots := make(map[int]bool)
			for _, m := range nm.metaMap {
				if m.Slot > 0 {
					usedSlots[m.Slot] = true
				}
			}
			for _, n := range nm.nodes {
				if n.Slot > 0 {
					usedSlots[n.Slot] = true
				}
			}
			s := 1
			for usedSlots[s] {
				s++
			}
			slot = s
			m := nm.metaMap[id]
			m.Slot = slot
			nm.metaMap[id] = m
			nm.saveMeta()
		}

		node = &Node{
			Slot:           slot,
			ID:             id,
			Hostname:       hostname,
			OSInfo:         osInfo,
			RemoteAddr:     addr.String(),
			udpAddr:        addr,
			LastSeen:       time.Now(),
			Cols:           80,
			Rows:           24,
			HasRoot:        isRoot,
			Note:           note,
			VncEngine:      vncEngine,
			VncPort:        vncPort,
			VncScale:       vncScale,
			VncTargetBpp:   vncBpp,
			DebugLogs:      []string{fmt.Sprintf("[%s] Node registered from %s (Slot #%d)", time.Now().Format("15:04:05"), addr.String(), slot)},
			seqCounter:       1,
			ptySubscribers:   make(map[string]*PTYSubscriber),
			vncSubscribers:   make(map[string]chan []byte),
			cmdChannels:      make(map[uint32]chan MsgCmdOutputPayload),
			fsListChannels:   make(map[uint32]chan []FsEntry),
			filePushChannels: make(map[uint32]chan uint8),
			filePullChannels: make(map[uint32]chan MsgFileChunkPayload),
		}
		nm.nodes[id] = node
	} else {
		node.mu.Lock()
		if hostname != "" {
			node.Hostname = hostname
		}
		if osInfo != "" {
			node.OSInfo = osInfo
		}
		if isRoot {
			node.HasRoot = true
		}
		node.RemoteAddr = addr.String()
		node.udpAddr = addr
		node.LastSeen = time.Now()
		node.mu.Unlock()
	}

	return node
}

// SetNodeNote updates the custom user label for a node and saves to disk
func (nm *NodeManager) SetNodeNote(id, note string) bool {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	meta := nm.metaMap[id]
	meta.Note = note
	nm.metaMap[id] = meta
	nm.saveMeta()

	if node, ok := nm.nodes[id]; ok {
		node.mu.Lock()
		node.Note = note
		node.mu.Unlock()
		node.AddLog(fmt.Sprintf("Label updated to: %s", note))
		return true
	}
	return true
}

// RemoveNode completely removes a node from active memory and saved metadata
func (nm *NodeManager) RemoveNode(id string) bool {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	delete(nm.nodes, id)
	delete(nm.metaMap, id)
	nm.saveMeta()
	return true
}

// SetNodeEngine updates preferred VNC engine for a node
func (nm *NodeManager) SetNodeEngine(id, engine string) bool {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	meta := nm.metaMap[id]
	meta.VncEngine = engine
	nm.metaMap[id] = meta
	nm.saveMeta()

	if node, ok := nm.nodes[id]; ok {
		node.mu.Lock()
		node.VncEngine = engine
		node.mu.Unlock()
		node.AddLog(fmt.Sprintf("VNC engine changed to: %s", engine))
		return true
	}
	return true
}

// SetNodePort updates preferred VNC port for a node
func (nm *NodeManager) SetNodePort(id string, port uint16) bool {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	meta := nm.metaMap[id]
	meta.VncPort = port
	nm.metaMap[id] = meta
	nm.saveMeta()

	if node, ok := nm.nodes[id]; ok {
		node.mu.Lock()
		node.VncPort = port
		node.mu.Unlock()
		node.AddLog(fmt.Sprintf("VNC port set to: %d", port))
		return true
	}
	return true
}

// SetNodeVncQuality updates resolution scale and color depth (BPP) for a node
func (nm *NodeManager) SetNodeVncQuality(id string, scale int, bpp int) bool {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	meta := nm.metaMap[id]
	if scale >= 0 {
		meta.VncScale = scale
	}
	if bpp == 8 || bpp == 16 || bpp == 32 {
		meta.VncBpp = bpp
	}
	nm.metaMap[id] = meta
	nm.saveMeta()

	if node, ok := nm.nodes[id]; ok {
		node.mu.Lock()
		if scale >= 0 {
			node.VncScale = scale
		}
		if bpp == 8 || bpp == 16 || bpp == 32 {
			node.VncTargetBpp = bpp
		}
		node.mu.Unlock()
		node.AddLog(fmt.Sprintf("VNC Quality updated: Scale=%dx, Bpp=%d", meta.VncScale, meta.VncBpp))
		return true
	}
	return true
}

func (nm *NodeManager) SetNodeVncScale(id string, scale int) bool {
	return nm.SetNodeVncQuality(id, scale, -1)
}

func (nm *NodeManager) SetNodeVncBpp(id string, bpp int) bool {
	return nm.SetNodeVncQuality(id, -1, bpp)
}

func (n *Node) GetVncScale() int {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.VncScale
}

func (n *Node) GetVncBpp() int {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.VncTargetBpp == 8 || n.VncTargetBpp == 16 || n.VncTargetBpp == 32 {
		return n.VncTargetBpp
	}
	return 16
}

// Touch updates LastSeen timestamp and address for a node
func (nm *NodeManager) Touch(id string, addr *net.UDPAddr) *Node {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	node, exists := nm.nodes[id]
	if exists {
		node.mu.Lock()
		node.RemoteAddr = addr.String()
		node.udpAddr = addr
		node.LastSeen = time.Now()
		node.mu.Unlock()
	}
	return node
}

// FindByAddr finds a node by its UDP address, with IP fallback for NAT port changes
func (nm *NodeManager) FindByAddr(addr *net.UDPAddr) *Node {
	nm.mu.RLock()
	targetStr := addr.String()
	targetIP := addr.IP.String()

	// 1. Exact match
	for _, n := range nm.nodes {
		n.mu.RLock()
		rem := n.RemoteAddr
		n.mu.RUnlock()
		if rem == targetStr {
			nm.mu.RUnlock()
			return n
		}
	}

	// 2. IP match fallback (NAT port change resilience)
	var matched *Node
	matchCount := 0
	for _, n := range nm.nodes {
		n.mu.RLock()
		var nIP string
		if n.udpAddr != nil {
			nIP = n.udpAddr.IP.String()
		}
		n.mu.RUnlock()
		if nIP == targetIP {
			matched = n
			matchCount++
		}
	}
	nm.mu.RUnlock()

	if matchCount == 1 {
		return matched
	}
	return nil
}

// GetNode retrieves a node by its unique ID
func (nm *NodeManager) GetNode(id string) (*Node, bool) {
	nm.mu.RLock()
	defer nm.mu.RUnlock()

	node, exists := nm.nodes[id]
	return node, exists
}

// GetNodeBySlot retrieves a node by its sequential slot number
func (nm *NodeManager) GetNodeBySlot(slot int) *Node {
	nm.mu.RLock()
	defer nm.mu.RUnlock()

	for _, n := range nm.nodes {
		if n.Slot == slot {
			return n
		}
	}
	return nil
}

// ListNodes returns all nodes sorted by Slot
func (nm *NodeManager) ListNodes() []*Node {
	nm.mu.RLock()
	defer nm.mu.RUnlock()

	res := make([]*Node, 0, len(nm.nodes))
	for _, n := range nm.nodes {
		res = append(res, n)
	}
	sort.Slice(res, func(i, j int) bool {
		return res[i].Slot < res[j].Slot
	})
	return res
}

// ListSummaries returns status summaries of all nodes sorted by Slot
func (nm *NodeManager) ListSummaries(onlineTimeout time.Duration) []*NodeSummary {
	nm.mu.RLock()
	nodesList := make([]*Node, 0, len(nm.nodes))
	for _, n := range nm.nodes {
		nodesList = append(nodesList, n)
	}
	nm.mu.RUnlock()

	sort.Slice(nodesList, func(i, j int) bool {
		return nodesList[i].Slot < nodesList[j].Slot
	})

	now := time.Now()
	res := make([]*NodeSummary, 0, len(nodesList))

	for _, n := range nodesList {
		n.mu.RLock()
		diff := now.Sub(n.LastSeen)
		isOnline := diff <= onlineTimeout

		res = append(res, &NodeSummary{
			Slot:            n.Slot,
			ID:              n.ID,
			Hostname:        n.Hostname,
			OSInfo:          n.OSInfo,
			RemoteAddr:      n.RemoteAddr,
			LastSeenAgoSec:  int64(diff.Seconds()),
			IsOnline:        isOnline,
			ActiveSessionID: n.ActiveSessionID,
			Cols:            n.Cols,
			Rows:            n.Rows,
			HasRoot:         n.HasRoot,
			VncActive:       n.VncActive,
			Note:            n.Note,
			VncEngine:       n.VncEngine,
			VncPort:         n.VncPort,
			VncScale:        n.VncScale,
			VncTargetBpp:    n.VncTargetBpp,
			ActiveVncClients: n.ActiveVncClients,
		})
		n.mu.RUnlock()
	}

	return res
}

// AddLog appends a timestamped log entry to the node (retains last 25)
func (n *Node) AddLog(msg string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	entry := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), msg)
	n.DebugLogs = append(n.DebugLogs, entry)
	if len(n.DebugLogs) > 25 {
		n.DebugLogs = n.DebugLogs[len(n.DebugLogs)-25:]
	}
}

// NextSeq increments and returns the next sequence number for outbound packets (lock-free)
func (n *Node) NextSeq() uint32 {
	return atomic.AddUint32(&n.seqCounter, 1)
}

// AddPTYSubscriber attaches a listener channel for incoming terminal output (all sessions)
func (n *Node) AddPTYSubscriber(subID string) chan []byte {
	return n.AddPTYSubscriberForSession(subID, 0)
}

// AddPTYSubscriberForSession attaches a listener channel for a specific session ID (or 0 for all)
func (n *Node) AddPTYSubscriberForSession(subID string, sessionID uint32) chan []byte {
	n.mu.Lock()
	defer n.mu.Unlock()

	ch := make(chan []byte, 128)
	n.ptySubscribers[subID] = &PTYSubscriber{
		Ch:        ch,
		SessionID: sessionID,
	}
	return ch
}

// RemovePTYSubscriber removes a listener channel
func (n *Node) RemovePTYSubscriber(subID string) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if sub, ok := n.ptySubscribers[subID]; ok {
		delete(n.ptySubscribers, subID)
		close(sub.Ch)
	}
}

// BroadcastPTY sends terminal bytes to all active listeners
func (n *Node) BroadcastPTY(data []byte) {
	n.BroadcastPTYSession(0, data)
}

// BroadcastPTYSession sends terminal bytes to listeners subscribed to sessionID (or all sessions if 0)
func (n *Node) BroadcastPTYSession(sessionID uint32, data []byte) {
	n.mu.RLock()
	defer n.mu.RUnlock()

	for _, sub := range n.ptySubscribers {
		if sub.SessionID == 0 || sessionID == 0 || sub.SessionID == sessionID {
			select {
			case sub.Ch <- data:
			default:
			}
		}
	}
}

// AddVNCSubscriber attaches a listener channel for incoming desktop stream tiles
func (n *Node) AddVNCSubscriber(subID string) chan []byte {
	n.mu.Lock()
	defer n.mu.Unlock()

	ch := make(chan []byte, 4096)
	n.vncSubscribers[subID] = ch
	return ch
}

// RemoveVNCSubscriber removes a desktop stream listener channel
func (n *Node) RemoveVNCSubscriber(subID string) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if ch, ok := n.vncSubscribers[subID]; ok {
		delete(n.vncSubscribers, subID)
		close(ch)
	}
}

// BroadcastVNC sends tile bytes to all desktop stream WebSocket listeners
func (n *Node) BroadcastVNC(data []byte) {
	n.mu.RLock()
	defer n.mu.RUnlock()

	for _, ch := range n.vncSubscribers {
		select {
		case ch <- data:
		default:
		}
	}
}

// UpdateFramebuffer writes a dirty tile into the node's full framebuffer
func (n *Node) UpdateFramebuffer(tx, ty, tw, th uint16, rawPixels []byte) {
	n.mu.RLock()
	w := n.VncWidth
	h := n.VncHeight
	n.mu.RUnlock()

	if w == 0 || h == 0 || tw == 0 || th == 0 {
		return
	}

	n.FullFbMu.Lock()
	defer n.FullFbMu.Unlock()

	totalBytes := int(w) * int(h) * 4
	if len(n.FullFb) != totalBytes {
		n.FullFb = make([]byte, totalBytes)
	}

	stride := int(w) * 4
	tileStride := int(tw) * 4
	for row := 0; row < int(th); row++ {
		dstY := int(ty) + row
		if dstY >= int(h) {
			break
		}
		dstOffset := dstY*stride + int(tx)*4
		srcOffset := row * tileStride
		if srcOffset+tileStride <= len(rawPixels) && dstOffset+tileStride <= len(n.FullFb) {
			copy(n.FullFb[dstOffset:dstOffset+tileStride], rawPixels[srcOffset:srcOffset+tileStride])
		}
	}
}

// GetFullFramebuffer returns a copy of the current full screen framebuffer
func (n *Node) GetFullFramebuffer() ([]byte, uint16, uint16) {
	n.mu.RLock()
	w := n.VncWidth
	h := n.VncHeight
	n.mu.RUnlock()

	n.FullFbMu.RLock()
	defer n.FullFbMu.RUnlock()

	if len(n.FullFb) == 0 || w == 0 || h == 0 {
		return nil, w, h
	}

	cpy := make([]byte, len(n.FullFb))
	copy(cpy, n.FullFb)
	return cpy, w, h
}

// Cmd Waiters
func (n *Node) AddCmdWaiter(reqID uint32) chan MsgCmdOutputPayload {
	n.mu.Lock()
	defer n.mu.Unlock()
	ch := make(chan MsgCmdOutputPayload, 64)
	n.cmdChannels[reqID] = ch
	return ch
}

func (n *Node) RemoveCmdWaiter(reqID uint32) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if ch, ok := n.cmdChannels[reqID]; ok {
		delete(n.cmdChannels, reqID)
		close(ch)
	}
}

func (n *Node) DispatchCmdOutput(p MsgCmdOutputPayload) {
	n.mu.RLock()
	ch, ok := n.cmdChannels[p.ReqID]
	n.mu.RUnlock()
	if ok {
		select {
		case ch <- p:
		default:
		}
	}
}

// FS List Waiters
func (n *Node) AddFsListWaiter(reqID uint32) chan []FsEntry {
	n.mu.Lock()
	defer n.mu.Unlock()
	ch := make(chan []FsEntry, 16)
	n.fsListChannels[reqID] = ch
	return ch
}

func (n *Node) RemoveFsListWaiter(reqID uint32) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if ch, ok := n.fsListChannels[reqID]; ok {
		delete(n.fsListChannels, reqID)
		close(ch)
	}
}

func (n *Node) DispatchFsList(reqID uint32, entries []FsEntry) {
	n.mu.RLock()
	ch, ok := n.fsListChannels[reqID]
	n.mu.RUnlock()
	if ok {
		select {
		case ch <- entries:
		default:
		}
	}
}

// File Push Waiters
func (n *Node) AddFilePushWaiter(transferID uint32) chan uint8 {
	n.mu.Lock()
	defer n.mu.Unlock()
	ch := make(chan uint8, 2)
	n.filePushChannels[transferID] = ch
	return ch
}

func (n *Node) RemoveFilePushWaiter(transferID uint32) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if ch, ok := n.filePushChannels[transferID]; ok {
		delete(n.filePushChannels, transferID)
		close(ch)
	}
}

func (n *Node) DispatchFilePushEnd(transferID uint32, status uint8) {
	n.mu.RLock()
	ch, ok := n.filePushChannels[transferID]
	n.mu.RUnlock()
	if ok {
		select {
		case ch <- status:
		default:
		}
	}
}

// File Pull Waiters
func (n *Node) AddFilePullWaiter(transferID uint32) chan MsgFileChunkPayload {
	n.mu.Lock()
	defer n.mu.Unlock()
	ch := make(chan MsgFileChunkPayload, 128)
	n.filePullChannels[transferID] = ch
	return ch
}

func (n *Node) RemoveFilePullWaiter(transferID uint32) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if ch, ok := n.filePullChannels[transferID]; ok {
		delete(n.filePullChannels, transferID)
		close(ch)
	}
}

func (n *Node) DispatchFilePullChunk(p MsgFileChunkPayload) {
	n.mu.RLock()
	ch, ok := n.filePullChannels[p.TransferID]
	n.mu.RUnlock()
	if ok {
		select {
		case ch <- p:
		default:
		}
	}
}
