package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	Mar4uderMagic   uint32 = 0x4D345244 // "M4RD"
	ProtocolVersion uint8  = 1
	MaxPayloadSize  int    = 1400
	HeaderSize      int    = 16
)

// Message types
const (
	MsgHeartbeat    uint8 = 0x01 // Keepalive ping/pong
	MsgRegister     uint8 = 0x02 // Node registration beacon
	MsgSessionOpen  uint8 = 0x03 // Server requests node to open/attach PTY
	MsgPtyData      uint8 = 0x04 // Raw terminal data (both directions)
	MsgPtyResize    uint8 = 0x05 // Terminal window resize (cols, rows)
	MsgSessionClose uint8 = 0x06 // Detach/close session

	// VNC / Remote Desktop Stream
	MsgVncStart uint8 = 0x10 // Request VNC stream start
	MsgVncStop  uint8 = 0x11 // Request VNC stream stop
	MsgVncData  uint8 = 0x12 // Framebuffer / tile data chunk
	MsgVncInput uint8 = 0x13 // Mouse & Keyboard input events

	// Native File & Command Operations (Zero external dependencies)
	MsgCmdExec       uint8 = 0x20 // Execute single isolated command
	MsgCmdOutput     uint8 = 0x21 // Output chunk / exit code
	MsgFsListReq     uint8 = 0x22 // Request directory contents
	MsgFsListResp    uint8 = 0x23 // Directory entries payload
	MsgFilePushStart uint8 = 0x24 // Server -> Node upload start
	MsgFilePushChunk uint8 = 0x25 // Server -> Node data chunk
	MsgFilePushEnd   uint8 = 0x26 // Server -> Node upload complete
	MsgFilePullReq   uint8 = 0x27 // Server -> Node download request
	MsgFilePullChunk uint8 = 0x28 // Node -> Server data chunk
	MsgFilePullEnd   uint8 = 0x29 // Node -> Server download complete

	// WebRTC / Ultra-Low-Latency Stream Transport
	MsgStreamStart  uint8 = 0x30 // Request stream start (RTSP/WebRTC)
	MsgStreamStop   uint8 = 0x31 // Request stream stop
	MsgStreamStatus uint8 = 0x32 // Stream status/telemetry
)

// PacketHeader represents the 16-byte fixed binary header
type PacketHeader struct {
	Magic     uint32 // 0x4D345244
	Version   uint8  // 1
	Type      uint8  // Msg*
	Flags     uint16 // Channel / options
	SessionID uint32 // Session ID
	Seq       uint32 // Sequence number
}

// MsgRegisterPayload represents the payload for MSG_REGISTER
type MsgRegisterPayload struct {
	NodeID   [64]byte
	Hostname [64]byte
	OSInfo   [64]byte
	ScreenW  uint16
	ScreenH  uint16
}

// MsgResize represents the payload for MSG_PTY_RESIZE and MSG_SESSION_OPEN
type MsgResize struct {
	Cols uint16
	Rows uint16
}

// VNC Input Event types
const (
	VncEventPointer uint8 = 0 // Pointer / Mouse
	VncEventKey     uint8 = 1 // Key
)

// MsgVncInputPayload represents mouse and keyboard input events
type MsgVncInputPayload struct {
	EventType  uint8  // 0 = mouse, 1 = key (VncEvent*)
	ButtonMask uint8  // mouse buttons (1=left, 2=middle, 4=right, 8=wheelup, 16=wheeldown)
	X          uint16 // pointer X (normalized 0..65535)
	Y          uint16 // pointer Y (normalized 0..65535)
	KeySym     uint32 // X11 key symbol
	DownFlag   uint8  // 1 = pressed, 0 = released
}

// MsgCmdExecPayload represents MSG_CMD_EXEC
type MsgCmdExecPayload struct {
	ReqID      uint32
	TimeoutSec uint32
	Command    [512]byte
}

// MsgCmdOutputPayload represents MSG_CMD_OUTPUT
type MsgCmdOutputPayload struct {
	ReqID    uint32
	ExitCode int32
	Len      uint16
	Data     [1024]byte
}

// MsgFsListReqPayload represents MSG_FS_LIST_REQ
type MsgFsListReqPayload struct {
	ReqID uint32
	Path  [512]byte
}

// FsEntry represents an entry inside MSG_FS_LIST_RESP
type FsEntry struct {
	IsDir uint8
	Mode  uint32
	Size  uint64
	Name  [256]byte
}

// MsgFilePushStartPayload represents MSG_FILE_PUSH_START
type MsgFilePushStartPayload struct {
	TransferID uint32
	TotalSize  uint64
	Mode       uint32
	Path       [512]byte
}

// MsgFileChunkPayload represents MSG_FILE_PUSH_CHUNK and MSG_FILE_PULL_CHUNK
type MsgFileChunkPayload struct {
	TransferID uint32
	Offset     uint64
	Len        uint16
	Data       [1024]byte
}

// MsgFileEndPayload represents MSG_FILE_PUSH_END and MSG_FILE_PULL_END
type MsgFileEndPayload struct {
	TransferID uint32
	Status     uint8
}

// MsgFilePullReqPayload represents MSG_FILE_PULL_REQ
type MsgFilePullReqPayload struct {
	TransferID uint32
	Path       [512]byte
}

// EncodeHeader encodes a PacketHeader to a 16-byte little-endian slice
func EncodeHeader(h PacketHeader) []byte {
	buf := make([]byte, HeaderSize)
	binary.LittleEndian.PutUint32(buf[0:4], h.Magic)
	buf[4] = h.Version
	buf[5] = h.Type
	binary.LittleEndian.PutUint16(buf[6:8], h.Flags)
	binary.LittleEndian.PutUint32(buf[8:12], h.SessionID)
	binary.LittleEndian.PutUint32(buf[12:16], h.Seq)
	return buf
}

// DecodeHeader decodes a 16-byte slice into a PacketHeader
func DecodeHeader(data []byte) (PacketHeader, error) {
	if len(data) < HeaderSize {
		return PacketHeader{}, errors.New("data too short for packet header")
	}
	h := PacketHeader{
		Magic:     binary.LittleEndian.Uint32(data[0:4]),
		Version:   data[4],
		Type:      data[5],
		Flags:     binary.LittleEndian.Uint16(data[6:8]),
		SessionID: binary.LittleEndian.Uint32(data[8:12]),
		Seq:       binary.LittleEndian.Uint32(data[12:16]),
	}
	if h.Magic != Mar4uderMagic {
		return h, fmt.Errorf("invalid magic: 0x%08X", h.Magic)
	}
	if h.Version != ProtocolVersion {
		return h, fmt.Errorf("unsupported protocol version: %d", h.Version)
	}
	return h, nil
}

// MsgStreamStartPayload represents payload for MsgStreamStart
type MsgStreamStartPayload struct {
	ServerHost [64]byte
	RtspPort   uint16
	StreamName [64]byte
	Width      uint16
	Height     uint16
	Fps        uint16
	BitrateKb  uint16
	Preset     uint8
	Reserved   [7]byte
}

// EncodeStreamStart encodes the payload into bytes
func EncodeStreamStart(serverHost string, rtspPort uint16, streamName string, width, height, fps, bitrateKb uint16, preset uint8) []byte {
	buf := new(bytes.Buffer)
	var hostBytes [64]byte
	copy(hostBytes[:], serverHost)
	_ = binary.Write(buf, binary.LittleEndian, hostBytes)
	_ = binary.Write(buf, binary.LittleEndian, rtspPort)
	var nameBytes [64]byte
	copy(nameBytes[:], streamName)
	_ = binary.Write(buf, binary.LittleEndian, nameBytes)
	_ = binary.Write(buf, binary.LittleEndian, width)
	_ = binary.Write(buf, binary.LittleEndian, height)
	_ = binary.Write(buf, binary.LittleEndian, fps)
	_ = binary.Write(buf, binary.LittleEndian, bitrateKb)
	_ = binary.Write(buf, binary.LittleEndian, preset)
	var reserved [7]byte
	_ = binary.Write(buf, binary.LittleEndian, reserved)
	return buf.Bytes()
}

// BuildPacket creates a complete packet with header and payload
func BuildPacket(msgType uint8, sessionID uint32, seq uint32, payload []byte) []byte {
	hdr := PacketHeader{
		Magic:     Mar4uderMagic,
		Version:   ProtocolVersion,
		Type:      msgType,
		Flags:     0,
		SessionID: sessionID,
		Seq:       seq,
	}
	pkt := EncodeHeader(hdr)
	if len(payload) > 0 {
		pkt = append(pkt, payload...)
	}
	return pkt
}

// CStringToString converts a null-terminated byte slice to a Go string
func CStringToString(b []byte) string {
	idx := bytes.IndexByte(b, 0)
	if idx >= 0 {
		return string(b[:idx])
	}
	return string(b)
}
