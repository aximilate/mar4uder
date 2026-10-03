package main

import (
	"bytes"
	"crypto/des"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"sync"
	"time"
)

// RFBPixelFormat describes pixel format requested by client via RFC 6143
type RFBPixelFormat struct {
	Bpp        byte
	Depth      byte
	BigEndian  byte
	TrueColor  byte
	RedMax     uint16
	GreenMax   uint16
	BlueMax    uint16
	RedShift   byte
	GreenShift byte
	BlueShift  byte
}

// RFBGateway provides standard RFC 6143 VNC server gateway on port 5900
type RFBGateway struct {
	listener net.Listener
	addr     string
	nm       *NodeManager
	udpGw    *UDPGateway
	cfg      *Config
	stats    *ServerStats
}

// NewRFBGateway creates and binds a standard RFB (VNC) server
func NewRFBGateway(addr string, nm *NodeManager, udpGw *UDPGateway, cfg *Config, stats *ServerStats) (*RFBGateway, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return &RFBGateway{
		listener: l,
		addr:     addr,
		nm:       nm,
		udpGw:    udpGw,
		cfg:      cfg,
		stats:    stats,
	}, nil
}

// vncEncryptChallenge encrypts 16-byte challenge with DES using RFC 6143 bit-reversed password key
func vncEncryptChallenge(challenge []byte, password string) []byte {
	var key [8]byte
	for i := 0; i < len(password) && i < 8; i++ {
		b := password[i]
		var rev byte
		for j := 0; j < 8; j++ {
			if (b & (1 << j)) != 0 {
				rev |= (1 << (7 - j))
			}
		}
		key[i] = rev
	}
	block, err := des.NewCipher(key[:])
	if err != nil {
		return nil
	}
	resp := make([]byte, 16)
	block.Encrypt(resp[0:8], challenge[0:8])
	block.Encrypt(resp[8:16], challenge[8:16])
	return resp
}

// Start begins accepting incoming VNC viewer connections
func (gw *RFBGateway) Start() {
	for {
		conn, err := gw.listener.Accept()
		if err != nil {
			return
		}
		go gw.handleClient(conn)
	}
}

func (gw *RFBGateway) handleClient(conn net.Conn) {
	gw.HandleNodeClient(conn, gw.getActiveNode())
}

// HandleNodeClient handles standard RFC 6143 RFB connection for a specific node
func (gw *RFBGateway) HandleNodeClient(conn net.Conn, targetNode *Node) {
	defer conn.Close()
	remoteAddr := conn.RemoteAddr().String()
	log.Printf("[RFB] Standard VNC Viewer connected from %s", remoteAddr)
	defer log.Printf("[RFB] Standard VNC Viewer disconnected from %s", remoteAddr)

	// 1. ProtocolVersion Handshake (RFB 003.008)
	if _, err := conn.Write([]byte("RFB 003.008\n")); err != nil {
		return
	}

	clientVer := make([]byte, 12)
	if _, err := io.ReadFull(conn, clientVer); err != nil {
		return
	}

	// 2. Security Handshake (Type 2 = VNC Authentication if password set, else Type 1 = None)
	pass := ""
	if gw.cfg != nil {
		pass = gw.cfg.GetAdminPassword()
	}

	if pass != "" {
		// Announce Security Type 2 (VNC Auth)
		if _, err := conn.Write([]byte{1, 2}); err != nil {
			return
		}

		secChoice := make([]byte, 1)
		if _, err := io.ReadFull(conn, secChoice); err != nil {
			return
		}
		if secChoice[0] != 2 {
			log.Printf("[RFB] Client %s rejected VNC Auth (chose %d)", remoteAddr, secChoice[0])
			return
		}

		challenge := make([]byte, 16)
		if _, err := rand.Read(challenge); err != nil {
			return
		}
		if _, err := conn.Write(challenge); err != nil {
			return
		}

		clientResp := make([]byte, 16)
		if _, err := io.ReadFull(conn, clientResp); err != nil {
			return
		}

		expected := vncEncryptChallenge(challenge, pass)
		if expected == nil || !bytes.Equal(clientResp, expected) {
			log.Printf("[RFB] Authentication FAILED from %s", remoteAddr)
			// SecurityResult: Failed (0x00000001)
			_, _ = conn.Write([]byte{0, 0, 0, 1})
			errMsg := "Authentication failed: invalid VNC password"
			errLen := make([]byte, 4)
			binary.BigEndian.PutUint32(errLen, uint32(len(errMsg)))
			_, _ = conn.Write(errLen)
			_, _ = conn.Write([]byte(errMsg))
			return
		}

		// SecurityResult: OK (0x00000000)
		if _, err := conn.Write([]byte{0, 0, 0, 0}); err != nil {
			return
		}
	} else {
		// Type 1 = None
		if _, err := conn.Write([]byte{1, 1}); err != nil {
			return
		}
		secChoice := make([]byte, 1)
		if _, err := io.ReadFull(conn, secChoice); err != nil {
			return
		}
		if _, err := conn.Write([]byte{0, 0, 0, 0}); err != nil {
			return
		}
	}

	// 3. ClientInit (shared flag)
	sharedFlag := make([]byte, 1)
	if _, err := io.ReadFull(conn, sharedFlag); err != nil {
		return
	}

	// Use specified target node or fallback to active node
	node := targetNode
	if node == nil {
		node = gw.getActiveNode()
	}

	if node != nil {
		node.mu.Lock()
		node.ActiveVncClients++
		node.mu.Unlock()

		defer func() {
			node.mu.Lock()
			if node.ActiveVncClients > 0 {
				node.ActiveVncClients--
			}
			remaining := node.ActiveVncClients
			node.mu.Unlock()

			if remaining == 0 {
				_ = gw.udpGw.SendVncStop(node)
			}
		}()

		// Start VNC stream on target node immediately
		_ = gw.udpGw.SendVncStart(node)

		// Wait up to 1500ms for node to attach and report real screen resolution
		for i := 0; i < 15; i++ {
			node.mu.RLock()
			hasRes := node.VncWidth > 0 && node.VncHeight > 0
			node.mu.RUnlock()
			if hasRes {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	wPx := uint16(1920)
	hPx := uint16(1080)
	nodeName := "MAR4UDER Remote Desktop"

	if node != nil {
		node.mu.RLock()
		if node.VncWidth > 0 && node.VncHeight > 0 {
			wPx = node.VncWidth
			hPx = node.VncHeight
		}
		nodeName = fmt.Sprintf("MAR4UDER // %s (%s)", node.ID, node.Hostname)
		node.mu.RUnlock()
	}

	// 4. Downscale resolution based on node quality setting or auto-detect
	origW := wPx
	origH := hPx
	scaleFactor := 1
	configuredScale := 0
	targetBpp := 16
	if node != nil {
		configuredScale = node.GetVncScale()
		targetBpp = node.GetVncBpp()
	}
	if configuredScale > 1 {
		scaleFactor = configuredScale
	} else if configuredScale == 1 {
		scaleFactor = 1 // explicitly 1:1
	} else {
		// auto mode: downscale 4K / UHD to HD
		if origW > 1920 || origH > 1080 {
			scaleFactor = 2
		}
	}

	if scaleFactor > 1 {
		wPx = origW / uint16(scaleFactor)
		hPx = origH / uint16(scaleFactor)
		log.Printf("[RFB] Downscaling node screen %dx%d -> %dx%d (%dx, %dbpp) for viewer %s", origW, origH, wPx, hPx, scaleFactor, targetBpp, remoteAddr)
	}

	// 5. ServerInit (Announce BPP according to quality settings: 8, 16, or 32)
	serverInit := make([]byte, 24+len(nodeName))
	binary.BigEndian.PutUint16(serverInit[0:2], wPx)
	binary.BigEndian.PutUint16(serverInit[2:4], hPx)

	var clientPF RFBPixelFormat
	if targetBpp == 8 {
		// 8bpp: 256 colors RGB332 (super compressed palette)
		serverInit[4] = 8  // bits-per-pixel
		serverInit[5] = 8  // depth
		serverInit[6] = 0  // little endian
		serverInit[7] = 1  // true-colour-flag
		binary.BigEndian.PutUint16(serverInit[8:10], 7)  // red-max (3 bits)
		binary.BigEndian.PutUint16(serverInit[10:12], 7) // green-max (3 bits)
		binary.BigEndian.PutUint16(serverInit[12:14], 3) // blue-max (2 bits)
		serverInit[14] = 5                               // red-shift
		serverInit[15] = 2                               // green-shift
		serverInit[16] = 0                               // blue-shift

		clientPF = RFBPixelFormat{
			Bpp: 8, Depth: 8, BigEndian: 0, TrueColor: 1,
			RedMax: 7, GreenMax: 7, BlueMax: 3,
			RedShift: 5, GreenShift: 2, BlueShift: 0,
		}
	} else if targetBpp == 32 {
		// 32bpp: Full 24-bit TrueColor
		serverInit[4] = 32
		serverInit[5] = 24
		serverInit[6] = 0
		serverInit[7] = 1
		binary.BigEndian.PutUint16(serverInit[8:10], 255)
		binary.BigEndian.PutUint16(serverInit[10:12], 255)
		binary.BigEndian.PutUint16(serverInit[12:14], 255)
		serverInit[14] = 16
		serverInit[15] = 8
		serverInit[16] = 0

		clientPF = RFBPixelFormat{
			Bpp: 32, Depth: 24, BigEndian: 0, TrueColor: 1,
			RedMax: 255, GreenMax: 255, BlueMax: 255,
			RedShift: 16, GreenShift: 8, BlueShift: 0,
		}
	} else {
		// Default: 16bpp RGB565
		serverInit[4] = 16
		serverInit[5] = 16
		serverInit[6] = 0
		serverInit[7] = 1
		binary.BigEndian.PutUint16(serverInit[8:10], 31)
		binary.BigEndian.PutUint16(serverInit[10:12], 63)
		binary.BigEndian.PutUint16(serverInit[12:14], 31)
		serverInit[14] = 11
		serverInit[15] = 5
		serverInit[16] = 0

		clientPF = RFBPixelFormat{
			Bpp: 16, Depth: 16, BigEndian: 0, TrueColor: 1,
			RedMax: 31, GreenMax: 63, BlueMax: 31,
			RedShift: 11, GreenShift: 5, BlueShift: 0,
		}
	}
	// 17, 18, 19 padding
	binary.BigEndian.PutUint32(serverInit[20:24], uint32(len(nodeName)))
	copy(serverInit[24:], []byte(nodeName))

	if _, err := conn.Write(serverInit); err != nil {
		return
	}

	if node == nil {
		return
	}

	var pfMu sync.RWMutex

	subID := fmt.Sprintf("rfb-%d", time.Now().UnixNano())
	vncCh := node.AddVNCSubscriber(subID)
	defer node.RemoveVNCSubscriber(subID)

	var writeMu sync.Mutex
	done := make(chan struct{})

	initialSent := false
	sendFullFramebuffer := func() {
		fb, fw, fh := node.GetFullFramebuffer()
		if len(fb) == 0 || fw == 0 || fh == 0 {
			return
		}

		renderFb := fb
		curW := fw
		curH := fh
		if scaleFactor > 1 {
			renderFb, curW, curH = downsampleScale(fb, int(fw), int(fh), scaleFactor)
		}

		if curW > wPx {
			curW = wPx
		}
		if curH > hPx {
			curH = hPx
		}

		writeMu.Lock()
		defer writeMu.Unlock()

		pfMu.RLock()
		pf := clientPF
		pfMu.RUnlock()

		bandH := 64
		for y := 0; y < int(curH); y += bandH {
			hChunk := bandH
			if y+hChunk > int(curH) {
				hChunk = int(curH) - y
			}

			stride := int(curW) * 4
			startOff := y * stride
			endOff := startOff + hChunk*stride
			if endOff > len(renderFb) {
				endOff = len(renderFb)
			}
			bandData := renderFb[startOff:endOff]
			convertedData := convertPixelsToClientFormat(bandData, (endOff-startOff)/4, pf)

			rfbHeader := make([]byte, 16)
			rfbHeader[0] = 0                              // FramebufferUpdate
			rfbHeader[1] = 0                              // padding
			binary.BigEndian.PutUint16(rfbHeader[2:4], 1) // 1 rect
			binary.BigEndian.PutUint16(rfbHeader[4:6], 0) // x
			binary.BigEndian.PutUint16(rfbHeader[6:8], uint16(y)) // y
			binary.BigEndian.PutUint16(rfbHeader[8:10], uint16(curW)) // w
			binary.BigEndian.PutUint16(rfbHeader[10:12], uint16(hChunk)) // h
			binary.BigEndian.PutUint32(rfbHeader[12:16], 0)            // Raw encoding (0)

			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := conn.Write(rfbHeader); err != nil {
				return
			}
			if _, err := conn.Write(convertedData); err != nil {
				return
			}
		}
		initialSent = true
	}

	// Goroutine: forward dirty tiles from agent to RFB viewer
	go func() {
		defer close(done)
		for tile := range vncCh {
			if len(tile) < 8 {
				continue
			}

			// Screen resolution update (0xFE 0xFE W H)
			if tile[0] == 0xFE && tile[1] == 0xFE {
				continue
			}

			if len(tile) < 12 {
				continue
			}

			tx := binary.BigEndian.Uint16(tile[0:2])
			ty := binary.BigEndian.Uint16(tile[2:4])
			tw := binary.BigEndian.Uint16(tile[4:6])
			th := binary.BigEndian.Uint16(tile[6:8])
			compFlag := tile[8]
			payloadLen := binary.BigEndian.Uint16(tile[10:12])

			if int(12+payloadLen) > len(tile) || tw == 0 || th == 0 {
				continue
			}

			payload := tile[12 : 12+payloadLen]
			rawPixels := decompressTile(payload, int(tw), int(th), compFlag)
			if len(rawPixels) == 0 {
				continue
			}

			outTx := tx
			outTy := ty
			outTw := tw
			outTh := th
			renderPixels := rawPixels

			inTileW := tw
			inTileH := th
			if scaleFactor > 1 {
				outTx = tx / uint16(scaleFactor)
				outTy = ty / uint16(scaleFactor)
				downscaled, dtw, dth := downsampleScale(rawPixels, int(tw), int(th), scaleFactor)
				outTw = uint16(dtw)
				outTh = uint16(dth)
				renderPixels = downscaled
				if outTw == 0 {
					outTw = 1
				}
				if outTh == 0 {
					outTh = 1
				}
				inTileW = outTw
				inTileH = outTh
			}

			// Strict bounds clamp to announced wPx, hPx to prevent TightVNC protocol error
			if outTx >= wPx || outTy >= hPx {
				continue
			}
			if outTx+outTw > wPx {
				outTw = wPx - outTx
			}
			if outTy+outTh > hPx {
				outTh = hPx - outTy
			}
			if outTw == 0 || outTh == 0 {
				continue
			}

			// If tile was clamped at the right/bottom border, crop rows cleanly
			if outTw < inTileW || outTh < inTileH {
				cropped := make([]byte, int(outTw)*int(outTh)*4)
				for r := 0; r < int(outTh); r++ {
					copy(cropped[r*int(outTw)*4:(r+1)*int(outTw)*4],
						renderPixels[r*int(inTileW)*4:r*int(inTileW)*4+int(outTw)*4])
				}
				renderPixels = cropped
			}

			pfMu.RLock()
			pf := clientPF
			pfMu.RUnlock()

			convertedPixels := convertPixelsToClientFormat(renderPixels, int(outTw)*int(outTh), pf)

			// RFB FramebufferUpdate:
			rfbHeader := make([]byte, 16)
			rfbHeader[0] = 0                             // FramebufferUpdate
			rfbHeader[1] = 0                             // padding
			binary.BigEndian.PutUint16(rfbHeader[2:4], 1) // 1 rect
			binary.BigEndian.PutUint16(rfbHeader[4:6], outTx)
			binary.BigEndian.PutUint16(rfbHeader[6:8], outTy)
			binary.BigEndian.PutUint16(rfbHeader[8:10], outTw)
			binary.BigEndian.PutUint16(rfbHeader[10:12], outTh)
			binary.BigEndian.PutUint32(rfbHeader[12:16], 0) // Raw encoding (0)

			rfbPacket := make([]byte, 16+len(convertedPixels))
			copy(rfbPacket[0:16], rfbHeader)
			copy(rfbPacket[16:], convertedPixels)

			writeMu.Lock()
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			_, err := conn.Write(rfbPacket)
			writeMu.Unlock()

			if err != nil {
				return
			}
		}
	}()

	// Loop reading RFB client messages (mouse, keyboard, pixel format, etc.)
	buf := make([]byte, 2048)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		msgType := make([]byte, 1)
		if _, err := io.ReadFull(conn, msgType); err != nil {
			break
		}

		switch msgType[0] {
		case 0: // SetPixelFormat (19 bytes: 3 pad + 16 pixel format)
			pad := make([]byte, 19)
			if _, err := io.ReadFull(conn, pad); err != nil {
				return
			}
			pfBuf := pad[3:]
			pfMu.Lock()
			clientPF.Bpp = pfBuf[0]
			clientPF.Depth = pfBuf[1]
			clientPF.BigEndian = pfBuf[2]
			clientPF.TrueColor = pfBuf[3]
			clientPF.RedMax = binary.BigEndian.Uint16(pfBuf[4:6])
			clientPF.GreenMax = binary.BigEndian.Uint16(pfBuf[6:8])
			clientPF.BlueMax = binary.BigEndian.Uint16(pfBuf[8:10])
			clientPF.RedShift = pfBuf[10]
			clientPF.GreenShift = pfBuf[11]
			clientPF.BlueShift = pfBuf[12]
			log.Printf("[RFB] Viewer %s set pixel format: %d bpp, depth %d, trueColor=%d, redMax=%d",
				remoteAddr, clientPF.Bpp, clientPF.Depth, clientPF.TrueColor, clientPF.RedMax)
			pfMu.Unlock()

		case 2: // SetEncodings (3 bytes + count*4)
			hdr := make([]byte, 3)
			if _, err := io.ReadFull(conn, hdr); err != nil {
				return
			}
			numEnc := binary.BigEndian.Uint16(hdr[1:3])
			if numEnc > 0 {
				encBuf := make([]byte, int(numEnc)*4)
				if _, err := io.ReadFull(conn, encBuf); err != nil {
					return
				}
			}

		case 3: // FramebufferUpdateRequest (9 bytes)
			reqBuf := make([]byte, 9)
			if _, err := io.ReadFull(conn, reqBuf); err != nil {
				return
			}
			incremental := reqBuf[0]
			if incremental == 0 || !initialSent {
				sendFullFramebuffer()
			}

		case 4: // KeyEvent (7 bytes)
			keyBuf := make([]byte, 7)
			if _, err := io.ReadFull(conn, keyBuf); err != nil {
				return
			}
			downFlag := keyBuf[0]
			keySym := binary.BigEndian.Uint32(keyBuf[3:7])
			_ = gw.udpGw.SendVncInput(node, MsgVncInputPayload{
				EventType: VncEventKey,
				DownFlag:  downFlag,
				KeySym:    keySym,
			})

		case 5: // PointerEvent (5 bytes)
			ptrBuf := make([]byte, 5)
			if _, err := io.ReadFull(conn, ptrBuf); err != nil {
				return
			}
			rawMask := ptrBuf[0]
			x := binary.BigEndian.Uint16(ptrBuf[1:3])
			y := binary.BigEndian.Uint16(ptrBuf[3:5])

			// Convert RFB button mask:
			buttonMask := byte(0)
			if (rawMask & 1) != 0 {
				buttonMask |= 1
			}
			if (rawMask & 2) != 0 {
				buttonMask |= 2
			}
			if (rawMask & 4) != 0 {
				buttonMask |= 4
			}
			if (rawMask & 8) != 0 {
				buttonMask |= 8 // Scroll Up
			}
			if (rawMask & 16) != 0 {
				buttonMask |= 16 // Scroll Down
			}

			// Pass normalized 0..65535 coordinates to agent
			inX := x
			inY := y
			if scaleFactor > 1 {
				inX = x * uint16(scaleFactor)
				inY = y * uint16(scaleFactor)
			}
			bw := 1920
			bh := 1080
			if node.VncWidth > 0 && node.VncHeight > 0 {
				bw = int(node.VncWidth)
				bh = int(node.VncHeight)
			}
			normX := math.Max(0, math.Min(1.0, float64(inX)/float64(bw)))
			normY := math.Max(0, math.Min(1.0, float64(inY)/float64(bh)))
			_ = gw.udpGw.SendVncInput(node, MsgVncInputPayload{
				EventType:  VncEventPointer,
				ButtonMask: buttonMask,
				X:          uint16(math.Round(normX * 65535.0)),
				Y:          uint16(math.Round(normY * 65535.0)),
			})

		case 6: // ClientCutText
			cutHdr := make([]byte, 7)
			if _, err := io.ReadFull(conn, cutHdr); err != nil {
				return
			}
			length := binary.BigEndian.Uint32(cutHdr[3:7])
			if length > 0 && length < 1000000 {
				textBuf := make([]byte, length)
				_, _ = io.ReadFull(conn, textBuf)
			}

		default:
			_, _ = conn.Read(buf)
		}
	}
}

func (gw *RFBGateway) getActiveNode() *Node {
	nodes := gw.nm.ListNodes()
	now := time.Now()
	for _, n := range nodes {
		n.mu.RLock()
		alive := now.Sub(n.LastSeen) <= 20*time.Second
		n.mu.RUnlock()
		if alive {
			return n
		}
	}
	if len(nodes) > 0 {
		return nodes[0]
	}
	return nil
}

func downsampleScale(src []byte, w, h int, scale int) ([]byte, uint16, uint16) {
	if scale <= 1 || w <= 1 || h <= 1 {
		return src, uint16(w), uint16(h)
	}
	dstW := w / scale
	dstH := h / scale
	if dstW < 1 {
		dstW = 1
	}
	if dstH < 1 {
		dstH = 1
	}
	dst := make([]byte, dstW*dstH*4)

	srcStride := w * 4
	dstStride := dstW * 4

	for y := 0; y < dstH; y++ {
		srcRow := (y * scale) * srcStride
		dstRow := y * dstStride
		for x := 0; x < dstW; x++ {
			sOff := srcRow + (x*scale)*4
			dOff := dstRow + x*4
			if sOff+3 < len(src) && dOff+3 < len(dst) {
				dst[dOff] = src[sOff]
				dst[dOff+1] = src[sOff+1]
				dst[dOff+2] = src[sOff+2]
				dst[dOff+3] = src[sOff+3]
			}
		}
	}
	return dst, uint16(dstW), uint16(dstH)
}

func convertPixelsToClientFormat(src []byte, count int, pf RFBPixelFormat) []byte {
	if count <= 0 || len(src) < count*4 {
		return src
	}
	if pf.Bpp == 32 && (pf.RedShift == 16 || pf.RedShift == 0) && pf.RedMax == 255 {
		// Standard 32-bit Little-Endian BGRA or RGBA
		return src[:count*4]
	}

	if pf.Bpp == 16 {
		// Convert 32bpp to 16bpp (e.g. RGB565 / RGB555)
		out := make([]byte, count*2)
		rMax := int(pf.RedMax)
		if rMax == 0 {
			rMax = 31
		}
		gMax := int(pf.GreenMax)
		if gMax == 0 {
			gMax = 63
		}
		bMax := int(pf.BlueMax)
		if bMax == 0 {
			bMax = 31
		}

		for i := 0; i < count; i++ {
			sIdx := i * 4
			b := int(src[sIdx])
			g := int(src[sIdx+1])
			r := int(src[sIdx+2])

			rScaled := (r * rMax) / 255
			gScaled := (g * gMax) / 255
			bScaled := (b * bMax) / 255

			val := uint16((rScaled << pf.RedShift) | (gScaled << pf.GreenShift) | (bScaled << pf.BlueShift))
			dIdx := i * 2
			if pf.BigEndian == 1 {
				out[dIdx] = byte(val >> 8)
				out[dIdx+1] = byte(val & 0xFF)
			} else {
				out[dIdx] = byte(val & 0xFF)
				out[dIdx+1] = byte(val >> 8)
			}
		}
		return out
	}

	if pf.Bpp == 8 {
		// Convert to 8bpp (RGB332 or grey)
		out := make([]byte, count)
		rMax := int(pf.RedMax)
		if rMax == 0 {
			rMax = 7
		}
		gMax := int(pf.GreenMax)
		if gMax == 0 {
			gMax = 7
		}
		bMax := int(pf.BlueMax)
		if bMax == 0 {
			bMax = 3
		}

		for i := 0; i < count; i++ {
			sIdx := i * 4
			b := int(src[sIdx])
			g := int(src[sIdx+1])
			r := int(src[sIdx+2])

			rScaled := (r * rMax) / 255
			gScaled := (g * gMax) / 255
			bScaled := (b * bMax) / 255

			out[i] = byte((rScaled << pf.RedShift) | (gScaled << pf.GreenShift) | (bScaled << pf.BlueShift))
		}
		return out
	}

	return src[:count*4]
}

func decompressTile(payload []byte, tw, th int, compFlag byte) []byte {
	totalPx := tw * th
	out := make([]byte, totalPx*4)

	if compFlag == 1 {
		inIdx := 0
		outPx := 0
		for inIdx+1 < len(payload) && outPx < totalPx {
			tag := binary.BigEndian.Uint16(payload[inIdx : inIdx+2])
			inIdx += 2

			if (tag & 0x8000) != 0 {
				run := int(tag & 0x7FFF)
				if inIdx+3 > len(payload) {
					break
				}
				b := payload[inIdx]
				g := payload[inIdx+1]
				r := payload[inIdx+2]
				inIdx += 4

				for i := 0; i < run && outPx < totalPx; i++ {
					pIdx := outPx * 4
					out[pIdx] = b
					out[pIdx+1] = g
					out[pIdx+2] = r
					out[pIdx+3] = 0xFF
					outPx++
				}
			} else {
				lit := int(tag)
				for i := 0; i < lit && outPx < totalPx; i++ {
					if inIdx+3 > len(payload) {
						break
					}
					b := payload[inIdx]
					g := payload[inIdx+1]
					r := payload[inIdx+2]
					inIdx += 4

					pIdx := outPx * 4
					out[pIdx] = b
					out[pIdx+1] = g
					out[pIdx+2] = r
					out[pIdx+3] = 0xFF
					outPx++
				}
			}
		}
	} else {
		inIdx := 0
		for i := 0; i < totalPx && inIdx+4 <= len(payload); i++ {
			pIdx := i * 4
			out[pIdx] = payload[inIdx]
			out[pIdx+1] = payload[inIdx+1]
			out[pIdx+2] = payload[inIdx+2]
			out[pIdx+3] = 0xFF
			inIdx += 4
		}
	}

	return out
}
