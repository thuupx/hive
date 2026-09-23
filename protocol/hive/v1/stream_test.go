package v1

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestStreamRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	s := NewStream(&buf, &buf)

	req, _ := NewRequest(NumberID(1), "session.create", nil)
	note, _ := NewNotification("session.update", map[string]string{"state": "running"})
	res, _ := NewResult(NumberID(1), map[string]string{"session_id": "sess_1"})

	for _, m := range []*Message{req, note, res} {
		if err := s.Write(m); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	want := []string{"session.create", "session.update", ""}
	for i, w := range want {
		got, err := s.Read()
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if got.Method != w {
			t.Errorf("read %d method = %q, want %q", i, got.Method, w)
		}
	}

	if _, err := s.Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("read past end = %v, want EOF", err)
	}
}

func TestStreamSkipsBlankLines(t *testing.T) {
	in := strings.NewReader("\n\n" + `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n\n")
	s := NewStream(in, io.Discard)
	m, err := s.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if m.Method != "ping" {
		t.Fatalf("method = %q", m.Method)
	}
}

func TestStreamRejectsOversizeLine(t *testing.T) {
	huge := `{"jsonrpc":"2.0","id":1,"method":"` + strings.Repeat("x", MaxMessageSize+16) + `"}`
	s := NewStream(strings.NewReader(huge), io.Discard)
	if _, err := s.Read(); err == nil {
		t.Fatal("expected an error for an oversize message")
	}
}

func TestStreamWriteValidates(t *testing.T) {
	s := NewStream(strings.NewReader(""), io.Discard)
	bad := &Message{JSONRPC: "1.0"}
	if err := s.Write(bad); err == nil {
		t.Fatal("expected validation error on write")
	}
}

func TestStreamWriteIsLineDelimited(t *testing.T) {
	var buf bytes.Buffer
	s := NewStream(strings.NewReader(""), &buf)
	m, _ := NewNotification("ping", nil)
	if err := s.Write(m); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := buf.String(); !strings.HasSuffix(got, "\n") || strings.Count(got, "\n") != 1 {
		t.Fatalf("output is not exactly one line: %q", got)
	}
}
