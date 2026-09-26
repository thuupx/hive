package v1_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

func TestStreamRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	s := v1.NewStream(&buf, &buf)

	req, _ := v1.NewRequest(v1.NumberID(1), "session.create", nil)
	note, _ := v1.NewNotification("session.update", map[string]string{"state": "running"})
	res, _ := v1.NewResult(v1.NumberID(1), map[string]string{"session_id": "sess_1"})

	for _, m := range []*v1.Message{req, note, res} {
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
	s := v1.NewStream(in, io.Discard)
	m, err := s.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if m.Method != "ping" {
		t.Fatalf("method = %q", m.Method)
	}
}

func TestStreamRejectsOversizeLine(t *testing.T) {
	huge := `{"jsonrpc":"2.0","id":1,"method":"` + strings.Repeat("x", v1.MaxMessageSize+16) + `"}`
	s := v1.NewStream(strings.NewReader(huge), io.Discard)
	if _, err := s.Read(); err == nil {
		t.Fatal("expected an error for an oversize message")
	}
}

func TestStreamWriteValidates(t *testing.T) {
	s := v1.NewStream(strings.NewReader(""), io.Discard)
	bad := &v1.Message{JSONRPC: "1.0"}
	if err := s.Write(bad); err == nil {
		t.Fatal("expected validation error on write")
	}
}

func TestStreamWriteIsLineDelimited(t *testing.T) {
	var buf bytes.Buffer
	s := v1.NewStream(strings.NewReader(""), &buf)
	m, _ := v1.NewNotification("ping", nil)
	if err := s.Write(m); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := buf.String(); !strings.HasSuffix(got, "\n") || strings.Count(got, "\n") != 1 {
		t.Fatalf("output is not exactly one line: %q", got)
	}
}
