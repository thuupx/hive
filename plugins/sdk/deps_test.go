package sdk_test

import (
	"context"
	"go/parser"
	"go/token"
	"io/fs"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/thupham/hive/plugins/sdk"
	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// TestNoCoreDependency enforces the plugin boundary: the plugin SDK is part of
// the public surface, so a plugin that uses it never links the Hive core.
func TestNoCoreDependency(t *testing.T) {
	skipTests := func(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", skipTests, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	found := 0
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, imp := range file.Imports {
				found++
				path := strings.Trim(imp.Path.Value, `"`)
				if path == "internal" || strings.HasPrefix(path, "internal/") ||
					strings.Contains(path, "/internal/") {
					t.Errorf("plugin sdk imports internal package %q", path)
				}
			}
		}
	}
	if found == 0 {
		t.Fatal("no imports found; package parsing did not work")
	}
}

// The core's shutdown request ends the run loop, so a plugin gets the chance to
// end what it started before the process goes away.
//
// Without it a plugin that spawned something — an agent process, for instance —
// left it running: the connection closed, the plugin exited, and the child was
// orphaned with nothing to collect it.
func TestShutdownRequestEndsTheRunLoop(t *testing.T) {
	pluginConn, coreConn := net.Pipe()
	defer pluginConn.Close()
	defer coreConn.Close()

	// The core side answers the handshake, so Connect can finish.
	core := v1.NewPeer(v1.NewStream(coreConn, coreConn, coreConn))
	core.Start()
	defer core.Close()

	go func() {
		for req := range core.Requests() {
			if req.Method == v1.MethodPluginHello {
				_ = core.Respond(req.RequestID(), v1.HelloResponse{
					InstanceID:           "inst_1",
					ConnectionGeneration: 1,
					Capabilities:         []string{v1.CapabilityExecutionWrite},
				})
			}
		}
	}()

	host, err := sdk.Connect(context.Background(),
		v1.NewStream(pluginConn, pluginConn, pluginConn), sdk.Options{
			ID:           "test",
			Type:         v1.PluginTypeAgent,
			Version:      "test",
			Capabilities: []string{v1.CapabilityExecutionWrite},
		})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- host.Run(context.Background()) }()

	// The core asks the plugin to stop, the way the supervisor does.
	if err := core.Notify(v1.MethodPluginShutdown, nil); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil for an ordinary shutdown", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the run loop did not end on a shutdown request")
	}
}
