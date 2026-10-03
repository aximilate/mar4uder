package main

import (
	"encoding/binary"
	"io"
	"log"
	"net"
)

// TCPGateway accepts reliable TCP connections for high-res VNC dirty-tile streaming
type TCPGateway struct {
	listener net.Listener
	nm       *NodeManager
	stats    *ServerStats
}

// NewTCPGateway creates a new TCP gateway
func NewTCPGateway(addr string, nm *NodeManager, stats *ServerStats) (*TCPGateway, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	return &TCPGateway{
		listener: l,
		nm:       nm,
		stats:    stats,
	}, nil
}

// Start begins accepting TCP stream connections from agents
func (gw *TCPGateway) Start() {
	for {
		conn, err := gw.listener.Accept()
		if err != nil {
			log.Printf("[-] TCP accept error: %v", err)
			return
		}
		go gw.handleVncConnection(conn)
	}
}

func (gw *TCPGateway) handleVncConnection(conn net.Conn) {
	defer conn.Close()

	// 1. Read 75-byte VNC handshake:
	// Magic (4B) + Version (1B) + Type (1B) + NodeID (64B) + Width (2B) + Height (2B) + Bpp (1B)
	handshake := make([]byte, 75)
	if _, err := io.ReadFull(conn, handshake); err != nil {
		return
	}

	magic := binary.LittleEndian.Uint32(handshake[0:4])
	if magic != Mar4uderMagic {
		return
	}

	nodeID := CStringToString(handshake[6:70])
	w := binary.BigEndian.Uint16(handshake[70:72])
	h := binary.BigEndian.Uint16(handshake[72:74])
	bpp := handshake[74]

	node, exists := gw.nm.GetNode(nodeID)
	if !exists {
		return
	}

	node.mu.Lock()
	node.VncActive = true
	node.VncWidth = w
	node.VncHeight = h
	node.VncBpp = bpp
	node.mu.Unlock()

	log.Printf("[+] VNC TCP stream attached for node %s (%dx%d, %d bpp)", nodeID, w, h, bpp)

	// Broadcast initial resolution message (JSON or binary) to active viewers
	initMsg := make([]byte, 8)
	initMsg[0] = 0xFE // Tag: Resolution update
	initMsg[1] = 0xFE
	binary.BigEndian.PutUint16(initMsg[2:4], w)
	binary.BigEndian.PutUint16(initMsg[4:6], h)
	initMsg[6] = bpp
	initMsg[7] = 0
	node.BroadcastVNC(initMsg)

	defer func() {
		node.mu.Lock()
		node.VncActive = false
		node.mu.Unlock()
		log.Printf("[*] VNC TCP stream closed for node %s", nodeID)
	}()

	// 2. Loop reading 12-byte tile headers + payloads
	headerBuf := make([]byte, 12)
	for {
		if _, err := io.ReadFull(conn, headerBuf); err != nil {
			break
		}

		payloadLen := binary.BigEndian.Uint16(headerBuf[10:12])
		tileBuf := make([]byte, 12+int(payloadLen))
		copy(tileBuf[0:12], headerBuf)

		if payloadLen > 0 {
			if _, err := io.ReadFull(conn, tileBuf[12:]); err != nil {
				break
			}
		}

		gw.stats.InPackets.Add(1)
		gw.stats.InBytes.Add(int64(len(tileBuf)))

		tx := binary.BigEndian.Uint16(headerBuf[0:2])
		ty := binary.BigEndian.Uint16(headerBuf[2:4])
		tw := binary.BigEndian.Uint16(headerBuf[4:6])
		th := binary.BigEndian.Uint16(headerBuf[6:8])
		compFlag := headerBuf[8]
		rawPixels := decompressTile(tileBuf[12:], int(tw), int(th), compFlag)
		if len(rawPixels) > 0 {
			node.UpdateFramebuffer(tx, ty, tw, th, rawPixels)
		}

		// Broadcast tile to all WebSocket desktop clients
		node.BroadcastVNC(tileBuf)
	}
}
