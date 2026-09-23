package logging

import (
	"io"
	"log/slog"
	"os"
)

// LogFileEnv names the log file every Hive process writes to.
//
// A plugin is its own process, so its log is its own stderr. That is where the
// transport says whether a delivery was received, whether it rendered, and where
// it went — the lines that answer "why was nothing answered" — and leaving them
// out of the installation's log hides exactly the thing someone is looking for.
//
// The daemon sets this for the processes it starts, so the whole installation
// writes one log.
const LogFileEnv = "HIVE_LOG_FILE"

// LogLevelEnv and LogFormatEnv carry the installation's log settings to a process
// the daemon started.
//
// A plugin logs the way the installation is configured, not the way its own
// defaults happen to be, so a user who turned debug on sees the transport's
// decisions too.
const (
	LogLevelEnv  = "HIVE_LOG_LEVEL"
	LogFormatEnv = "HIVE_LOG_FORMAT"
)

// NewForProcess builds a logger for a process the daemon started.
//
// The file is appended to rather than rotated here: the daemon owns rotation,
// because it is the one that runs longest and writes most. A file that rotates
// while this process holds it open keeps this process's output in the rotated
// copy, which is kept anyway.
func NewForProcess() (*slog.Logger, func()) {
	level := os.Getenv(LogLevelEnv)
	format := os.Getenv(LogFormatEnv)
	path := os.Getenv(LogFileEnv)
	if path == "" {
		return New(os.Stderr, level, format), func() {}
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		// A log file that cannot be opened is not a reason to refuse to run: the
		// terminal still has the log.
		return New(os.Stderr, level, format), func() {}
	}

	return New(io.MultiWriter(os.Stderr, file), level, format), func() { _ = file.Close() }
}
