package v1

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// MaxMessageSize bounds a single protocol message.
const MaxMessageSize = 4 << 20 // 4 MiB

// Stream reads and writes newline-delimited JSON-RPC messages.
//
// The stdio transport used by plugins and local agents frames one message
// per line. A WebSocket transport carries one message per frame and does
// not need this type.
type Stream struct {
	r *bufio.Scanner
	w io.Writer
}

// NewStream returns a Stream reading from r and writing to w.
func NewStream(r io.Reader, w io.Writer) *Stream {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), MaxMessageSize)
	return &Stream{r: sc, w: w}
}

// Read returns the next message. Blank lines are skipped. It returns
// io.EOF at end of stream.
func (s *Stream) Read() (*Message, error) {
	for s.r.Scan() {
		line := s.r.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		return ParseMessage(line)
	}
	if err := s.r.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

// Write sends one message followed by a newline.
func (s *Stream) Write(m *Message) error {
	if err := m.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("hive: encode message: %w", err)
	}
	if len(b) > MaxMessageSize {
		return fmt.Errorf("hive: message exceeds %d bytes", MaxMessageSize)
	}
	if _, err := s.w.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}
