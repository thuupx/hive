package logging_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thuupx/hive/internal/logging"
)

// A daemon meant to run for months cannot write to one file forever.
func TestTheLogFileRotates(t *testing.T) {
	dir := t.TempDir()

	file, err := logging.OpenFileWith(dir, 512, 2)
	if err != nil {
		t.Fatalf("OpenFileWith: %v", err)
	}
	defer file.Close()

	record := []byte(strings.Repeat("a record that fills the file\n", 4))
	for i := 0; i < 40; i++ {
		if _, err := file.Write(record); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	names := map[string]bool{}
	for _, entry := range entries {
		names[entry.Name()] = true
	}
	if !names[logging.LogFileName] {
		t.Errorf("the current log is missing: %v", names)
	}
	if !names[logging.LogFileName+".1"] {
		t.Errorf("nothing was rotated: %v", names)
	}
	// The limit is kept: the oldest goes before a new one is made.
	if names[logging.LogFileName+".3"] {
		t.Errorf("more files were kept than asked for: %v", names)
	}

	info, err := os.Stat(filepath.Join(dir, logging.LogFileName))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() > 512+int64(len(record)) {
		t.Errorf("the current file grew to %d, past the limit", info.Size())
	}
}

// The log is written by the daemon and readable only by its owner.
func TestTheLogFileIsPrivate(t *testing.T) {
	dir := t.TempDir()

	file, err := logging.OpenFile(dir)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer file.Close()

	if _, err := file.Write([]byte("a line\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	info, err := os.Stat(file.Path())
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("the log is %#o, want it readable by its owner only", info.Mode().Perm())
	}
}

// Opening a directory that does not exist yet is what a first run does.
func TestOpenCreatesTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "log")

	file, err := logging.OpenFile(dir)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer file.Close()

	if _, err := os.Stat(file.Path()); err != nil {
		t.Fatalf("the log was not created: %v", err)
	}
}
