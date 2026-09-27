package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/thuupx/hive/plugins/sdk"
	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

// manifestTimeout bounds how long a plugin gets to describe itself.
//
// The core asks a plugin what it needs without starting it, and a plugin that
// does not answer promptly must not hold up `hive init`, `hive doctor`, or
// `hive service install`.
const manifestTimeout = 5 * time.Second

// pluginManifests returns the manifest of every plugin binary in dir.
//
// The core discovers plugins by name, so it asks each one what it needs rather
// than knowing it in advance. A binary that cannot describe itself is skipped: a
// plugin that is absent or broken must not stop a command that only wants to
// read the ones that are there.
func pluginManifests(dir string) map[string]v1.PluginManifest {
	out := map[string]v1.PluginManifest{}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "hive-plugin-") {
			continue
		}
		name := strings.TrimPrefix(entry.Name(), "hive-plugin-")

		manifest, err := queryManifest(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		if manifest.ID == "" {
			manifest.ID = name
		}
		out[manifest.ID] = manifest
	}
	return out
}

// queryManifest asks one plugin binary what it needs.
func queryManifest(binary string) (v1.PluginManifest, error) {
	ctx, cancel := context.WithTimeout(context.Background(), manifestTimeout)
	defer cancel()

	output, err := exec.CommandContext(ctx, binary, "-"+sdk.DescribeFlag).Output()
	if err != nil {
		return v1.PluginManifest{}, fmt.Errorf("describe %s: %w", filepath.Base(binary), err)
	}

	var manifest v1.PluginManifest
	if err := json.Unmarshal(output, &manifest); err != nil {
		return v1.PluginManifest{}, fmt.Errorf("describe %s: %w", filepath.Base(binary), err)
	}
	return manifest, nil
}

// transportManifests lists the transport plugins found in dir, in a stable order.
func transportManifests(dir string) []v1.PluginManifest {
	manifests := pluginManifests(dir)

	out := make([]v1.PluginManifest, 0, len(manifests))
	for _, manifest := range manifests {
		if manifest.Type == v1.PluginTypeTransport {
			out = append(out, manifest)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
