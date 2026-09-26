package control

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"

	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

// SocketFile is the Control API socket name inside the data directory.
const SocketFile = "hive.sock"

// maxSocketPath is a conservative bound on a unix socket path. The platform
// limit is around 104 bytes and varies, so leave room.
const maxSocketPath = 100

// ListenUnix starts a Control API listener on a unix socket.
//
// The socket is created with owner-only permissions. On a local machine the
// operating system authenticates the caller, so the principal is the owner and
// no credential is needed on the wire.
func ListenUnix(path string) (net.Listener, error) {
	// A unix socket path is bounded by the platform's sockaddr_un, which is
	// around 104 bytes. Failing here names the cause instead of leaving a bare
	// bind error.
	if len(path) > maxSocketPath {
		return nil, fmt.Errorf(
			"control: socket path is %d bytes, which exceeds the %d byte limit; use a shorter data directory",
			len(path), maxSocketPath)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("control: create %s: %w", filepath.Dir(path), err)
	}

	// A stale socket from a previous run would make Listen fail. Only remove a
	// socket, never a regular file.
	if info, err := os.Stat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("control: remove the stale socket %s: %w", path, err)
		}
	}

	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("control: listen on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("control: restrict %s: %w", path, err)
	}
	return listener, nil
}

// ServeUnix accepts Control API connections until ctx is cancelled.
//
// Every connection is served as the given connection identity. A unix socket on
// a personal machine has exactly one trust domain, so the owner identity is
// resolved once rather than negotiated per connection.
func ServeUnix(ctx context.Context, listener net.Listener, gateway *Gateway, conn Connection, log *slog.Logger) error {
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	for {
		netConn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("control: accept: %w", err)
		}
		go serveConn(ctx, netConn, gateway, conn, log)
	}
}

func serveConn(ctx context.Context, netConn net.Conn, gateway *Gateway, conn Connection, log *slog.Logger) {
	defer netConn.Close()

	stream := v1.NewStream(netConn, netConn, netConn)
	peer := v1.NewPeer(stream)
	peer.Start()
	defer peer.Close()

	if err := gateway.Serve(ctx, peer, conn); err != nil {
		log.Debug("control connection ended", "error", err)
	}
}
