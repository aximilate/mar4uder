package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	wsOpContinuation = 0x0
	wsOpText         = 0x1
	wsOpBinary       = 0x2
	wsOpClose        = 0x8
	wsOpPing         = 0x9
	wsOpPong         = 0xA
	wsGUID           = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
)

// WSConn wraps a hijacked net.Conn as an RFC 6455 WebSocket connection
type WSConn struct {
	conn     net.Conn
	bufrw    *bufio.ReadWriter
	writeMu  sync.Mutex
	isClosed bool
}

// UpgradeWebSocket upgrades an HTTP connection to WebSocket
func UpgradeWebSocket(w http.ResponseWriter, r *http.Request) (*WSConn, error) {
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if key == "" {
		http.Error(w, "Missing Sec-WebSocket-Key", http.StatusBadRequest)
		return nil, errors.New("missing Sec-WebSocket-Key")
	}

	h := sha1.New()
	h.Write([]byte(key + wsGUID))
	accept := base64.StdEncoding.EncodeToString(h.Sum(nil))

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Webserver doesn't support hijacking", http.StatusInternalServerError)
		return nil, errors.New("hijack unsupported")
	}

	conn, bufrw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	_ = conn.SetReadDeadline(time.Time{})
	_ = conn.SetWriteDeadline(time.Time{})

	res := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"

	if _, err := bufrw.WriteString(res); err != nil {
		conn.Close()
		return nil, err
	}
	if err := bufrw.Flush(); err != nil {
		conn.Close()
		return nil, err
	}

	return &WSConn{
		conn:  conn,
		bufrw: bufrw,
	}, nil
}

// ReadMessage reads the next message (text or binary) from the WebSocket
func (ws *WSConn) ReadMessage() (messageType int, payload []byte, err error) {
	for {
		b0, err := ws.bufrw.ReadByte()
		if err != nil {
			return 0, nil, err
		}
		opcode := int(b0 & 0x0F)

		b1, err := ws.bufrw.ReadByte()
		if err != nil {
			return 0, nil, err
		}

		isMasked := (b1 & 0x80) != 0
		lenField := int(b1 & 0x7F)

		var length int64
		if lenField <= 125 {
			length = int64(lenField)
		} else if lenField == 126 {
			var l16 uint16
			if err := binary.Read(ws.bufrw, binary.BigEndian, &l16); err != nil {
				return 0, nil, err
			}
			length = int64(l16)
		} else if lenField == 127 {
			var l64 uint64
			if err := binary.Read(ws.bufrw, binary.BigEndian, &l64); err != nil {
				return 0, nil, err
			}
			length = int64(l64)
		}

		var mask [4]byte
		if isMasked {
			if _, err := io.ReadFull(ws.bufrw, mask[:]); err != nil {
				return 0, nil, err
			}
		}

		data := make([]byte, length)
		if _, err := io.ReadFull(ws.bufrw, data); err != nil {
			return 0, nil, err
		}

		if isMasked {
			for i := range data {
				data[i] ^= mask[i%4]
			}
		}

		switch opcode {
		case wsOpPing:
			_ = ws.writeFrame(wsOpPong, data)
			continue
		case wsOpPong:
			continue
		case wsOpClose:
			_ = ws.writeFrame(wsOpClose, nil)
			ws.Close()
			return wsOpClose, nil, io.EOF
		case wsOpText, wsOpBinary:
			return opcode, data, nil
		default:
			// Ignore other frames
			continue
		}
	}
}

// WriteText sends a text frame
func (ws *WSConn) WriteText(text string) error {
	return ws.writeFrame(wsOpText, []byte(text))
}

// WriteBinary sends a binary frame
func (ws *WSConn) WriteBinary(data []byte) error {
	return ws.writeFrame(wsOpBinary, data)
}

func (ws *WSConn) writeFrame(opcode int, payload []byte) error {
	ws.writeMu.Lock()
	defer ws.writeMu.Unlock()

	if ws.isClosed {
		return errors.New("connection closed")
	}

	length := len(payload)
	header := []byte{byte(0x80 | (opcode & 0x0F))}

	if length <= 125 {
		header = append(header, byte(length))
	} else if length <= 65535 {
		header = append(header, 126, byte(length>>8), byte(length&0xFF))
	} else {
		buf := make([]byte, 10)
		buf[0] = byte(0x80 | (opcode & 0x0F))
		buf[1] = 127
		binary.BigEndian.PutUint64(buf[2:], uint64(length))
		header = buf
	}

	frame := make([]byte, len(header)+length)
	copy(frame, header)
	if length > 0 {
		copy(frame[len(header):], payload)
	}

	if _, err := ws.conn.Write(frame); err != nil {
		return err
	}
	return nil
}

// Close closes the WebSocket connection
func (ws *WSConn) Close() error {
	ws.writeMu.Lock()
	defer ws.writeMu.Unlock()
	if !ws.isClosed {
		ws.isClosed = true
		return ws.conn.Close()
	}
	return nil
}
