package wire

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Native messaging is how Chrome (and Edge, Brave...) lets an extension talk
// to a program on the computer. The browser starts the program and talks to
// it over stdin/stdout. Each message is JSON, preceded by its length as a
// 4-byte number (little-endian: lowest byte first).
//
//	[0x1d 0x00 0x00 0x00] {"type":"ping","url":"..."}
//
// Messages from the program to the browser may be at most 1 MB.

// Message is what the extension sends.
type Message struct {
	Type string `json:"type"` // "ping", "open", "add" or "video"
	Request
}

const maxMessage = 1 << 20

// ReadMessage reads one message from the browser.
func ReadMessage(r io.Reader) (Message, error) {
	var m Message
	var size uint32
	if err := binary.Read(r, binary.LittleEndian, &size); err != nil {
		return m, err
	}
	if size > maxMessage {
		return m, fmt.Errorf("message too big (%d bytes)", size)
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, errors.New("message isn't valid JSON")
	}
	return m, nil
}

// WriteMessage sends one message to the browser.
func WriteMessage(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(data) > maxMessage {
		return errors.New("reply too big")
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(len(data))); err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}
