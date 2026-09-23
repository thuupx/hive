package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// A daemon writes a log file as well as the terminal.
//
// A terminal is where a log goes when someone is watching, and a daemon is not
// watched. Without a file, "what happened an hour ago" has no answer, and a
// service that was started by the system has no terminal at all.
const (
	// LogFileName is the file the daemon writes.
	LogFileName = "hive.log"

	// DefaultMaxBytes is how large the file may grow before it is rotated.
	DefaultMaxBytes = 8 << 20

	// DefaultKeep is how many rotated files are kept.
	DefaultKeep = 3
)

// File is a log file that does not grow without limit.
//
// A process meant to run for months cannot write to one file forever: the disk
// would fill, and the interesting part of the log would be buried. Rotation keeps
// the recent past and drops the rest.
type File struct {
	dir      string
	maxBytes int64
	keep     int

	mu   sync.Mutex
	file *os.File
	size int64
}

// OpenFile opens the daemon's log in dir, creating the directory if it is not
// there.
func OpenFile(dir string) (*File, error) {
	return OpenFileWith(dir, DefaultMaxBytes, DefaultKeep)
}

// OpenFileWith opens a log file with explicit limits.
func OpenFileWith(dir string, maxBytes int64, keep int) (*File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("logging: %s: %w", dir, err)
	}

	f := &File{dir: dir, maxBytes: maxBytes, keep: keep}
	if err := f.open(); err != nil {
		return nil, err
	}
	return f, nil
}

// Path is the file being written.
func (f *File) Path() string { return filepath.Join(f.dir, LogFileName) }

// open opens the file for appending and remembers its size.
func (f *File) open() error {
	file, err := os.OpenFile(f.Path(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("logging: %s: %w", f.Path(), err)
	}

	info, err := file.Stat()
	if err != nil {
		file.Close()
		return fmt.Errorf("logging: %s: %w", f.Path(), err)
	}

	f.file = file
	f.size = info.Size()
	return nil
}

// Write appends a record, rotating first if it would not fit.
func (f *File) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.file == nil {
		return 0, os.ErrClosed
	}

	if f.maxBytes > 0 && f.size+int64(len(p)) > f.maxBytes {
		if err := f.rotate(); err != nil {
			// A rotation that fails must not lose the record: writing it to a large
			// file is better than not writing it at all.
			_ = err
		}
	}

	n, err := f.file.Write(p)
	f.size += int64(n)
	return n, err
}

// rotate moves the file aside and starts a new one.
//
// The caller holds the lock.
func (f *File) rotate() error {
	if err := f.file.Close(); err != nil {
		return err
	}

	// The oldest goes first, so the numbering stays contiguous.
	oldest := fmt.Sprintf("%s.%d", f.Path(), f.keep)
	_ = os.Remove(oldest)

	for i := f.keep - 1; i >= 1; i-- {
		from := fmt.Sprintf("%s.%d", f.Path(), i)
		to := fmt.Sprintf("%s.%d", f.Path(), i+1)
		_ = os.Rename(from, to)
	}
	_ = os.Rename(f.Path(), f.Path()+".1")

	return f.open()
}

// Close closes the file.
func (f *File) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.file == nil {
		return nil
	}
	err := f.file.Close()
	f.file = nil
	return err
}
