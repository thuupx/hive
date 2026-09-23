package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thupham/hive/internal/config"
	"github.com/thupham/hive/internal/logging"
)

// daemonLogger is where the daemon writes.
//
// A log file that cannot be opened is not a reason to refuse to start: the
// terminal still has the log, and a daemon that will not run because it cannot
// write a file is worse than one whose history is short.
func daemonLogger(cfg config.Config) (*slog.Logger, func()) {
	dir, err := cfg.EffectiveDataDir()
	if err != nil {
		return logging.New(os.Stderr, cfg.Log.Level, cfg.Log.Format), func() {}
	}

	file, err := logging.OpenFile(filepath.Join(dir, "log"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "hive: the log file could not be opened: %v\n", err)
		return logging.New(os.Stderr, cfg.Log.Level, cfg.Log.Format), func() {}
	}

	// Both, because a daemon in a terminal should still say what it is doing there.
	both := io.MultiWriter(os.Stderr, file)
	return logging.New(both, cfg.Log.Level, cfg.Log.Format), func() { _ = file.Close() }
}

// runLogs prints what the daemon has been doing.
func runLogs(f flags, args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	lines := fs.Int("lines", 50, "how many lines to show (0 for all)")
	follow := fs.Bool("follow", false, "keep printing as the daemon writes")
	level := fs.String("level", "", "only lines at this level or above")
	match := fs.String("grep", "", "only lines containing this text")
	if err := parseArgsAndFlags(fs, args); err != nil {
		return err
	}

	cfg, err := loadConfig(f)
	if err != nil {
		return err
	}
	dir, err := cfg.EffectiveDataDir()
	if err != nil {
		return err
	}

	path := filepath.Join(dir, "log", logging.LogFileName)
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no log file at %s\n"+
				"  the daemon writes one while it runs; `hive service install` keeps it across restarts", path)
		}
		return err
	}
	defer file.Close()

	// A tail of a file being appended to needs the last lines, which means reading
	// all of it: a log has no index.
	kept, err := tailLines(file, *lines, *level, *match)
	if err != nil {
		return err
	}
	for _, line := range kept {
		fmt.Println(line)
	}

	if !*follow {
		return nil
	}
	return followFile(context.Background(), path, file, *level, *match)
}

// tailLines reads the file and returns the last lines that pass the filters.
func tailLines(file *os.File, lines int, level, match string) ([]string, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	var kept []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !linePasses(line, level, match) {
			continue
		}
		kept = append(kept, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	if lines > 0 && len(kept) > lines {
		kept = kept[len(kept)-lines:]
	}
	return kept, nil
}

// followFile prints what is written after this moment.
func followFile(ctx context.Context, path string, file *os.File, level, match string) error {
	// Start at the end: the tail has already been printed.
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		return err
	}

	reader := bufio.NewReader(file)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\n")
			if linePasses(trimmed, level, match) {
				fmt.Println(trimmed)
			}
		}

		switch {
		case err == nil:
			continue
		case errors.Is(err, io.EOF):
			// A rotated file is a different file, so the name is reopened rather
			// than followed by descriptor.
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(250 * time.Millisecond):
			}
			if reopened, rerr := reopen(path, file); rerr == nil && reopened != nil {
				file = reopened
				reader = bufio.NewReader(file)
			}
		default:
			return err
		}
	}
}

// reopen follows a file across rotation.
func reopen(path string, current *os.File) (*os.File, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	if opened, err := current.Stat(); err == nil && os.SameFile(info, opened) {
		return nil, nil
	}

	return os.Open(path)
}

// linePasses reports whether a log line survives the filters.
func linePasses(line, level, match string) bool {
	if match != "" && !strings.Contains(line, match) {
		return false
	}
	if level == "" {
		return true
	}

	// The records carry their level, so a filter is a comparison rather than a
	// second logging format.
	want := logging.ParseLevel(level)
	for _, candidate := range []struct {
		text  string
		level slog.Level
	}{
		{"level=ERROR", slog.LevelError},
		{"level=WARN", slog.LevelWarn},
		{"level=INFO", slog.LevelInfo},
		{"level=DEBUG", slog.LevelDebug},
	} {
		if strings.Contains(line, candidate.text) {
			return candidate.level >= want
		}
	}

	// A line with no level of its own is part of a record — an agent's own output,
	// for instance — and dropping it would split one record in half.
	return true
}
