// Package dsi implements the Data Stream Interface, the framing protocol
// that carries the Apple Filing Protocol over TCP (port 548).
//
// The connection type multiplexes concurrent requests: many requests can
// be in flight at once and replies are matched to callers by request ID.
package dsi

import "encoding/binary"

// DSI command codes.
const (
	CmdCloseSession uint8 = 1
	CmdCommand      uint8 = 2
	CmdGetStatus    uint8 = 3
	CmdOpenSession  uint8 = 4
	CmdTickle       uint8 = 5
	CmdWrite        uint8 = 6
	CmdAttention    uint8 = 8
)

const (
	flagRequest uint8 = 0
	flagReply   uint8 = 1
)

// HeaderSize is the size of a DSI packet header on the wire.
const HeaderSize = 16

// Header is a DSI packet header.
type Header struct {
	Flags     uint8
	Command   uint8
	RequestID uint16
	// ErrCode holds the AFP result code in replies, and the payload
	// offset of the written data in DSIWrite requests.
	ErrCode uint32
	// Length is the number of payload bytes following the header.
	Length   uint32
	Reserved uint32
}

func (h Header) encode() [HeaderSize]byte {
	var b [HeaderSize]byte
	b[0] = h.Flags
	b[1] = h.Command
	binary.BigEndian.PutUint16(b[2:4], h.RequestID)
	binary.BigEndian.PutUint32(b[4:8], h.ErrCode)
	binary.BigEndian.PutUint32(b[8:12], h.Length)
	binary.BigEndian.PutUint32(b[12:16], h.Reserved)
	return b
}

func decodeHeader(b [HeaderSize]byte) Header {
	return Header{
		Flags:     b[0],
		Command:   b[1],
		RequestID: binary.BigEndian.Uint16(b[2:4]),
		ErrCode:   binary.BigEndian.Uint32(b[4:8]),
		Length:    binary.BigEndian.Uint32(b[8:12]),
		Reserved:  binary.BigEndian.Uint32(b[12:16]),
	}
}
