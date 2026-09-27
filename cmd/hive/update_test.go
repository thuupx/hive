package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
)

// releaseTarball builds an archive shaped like a release's.
func releaseTarball(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		content := files[name]
		if err := tw.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     0o755,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("tar write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

func TestReleaseAsset(t *testing.T) {
	for _, tc := range []struct {
		goos, goarch string
		want         string
		wantErr      bool
	}{
		{"linux", "amd64", "hive_1.2.3_linux_amd64.tar.gz", false},
		{"linux", "arm64", "hive_1.2.3_linux_arm64.tar.gz", false},
		{"darwin", "arm64", "hive_1.2.3_darwin_arm64.tar.gz", false},
		{"darwin", "amd64", "", true},
		{"windows", "amd64", "", true},
		{"linux", "riscv64", "", true},
	} {
		t.Run(tc.goos+"/"+tc.goarch, func(t *testing.T) {
			got, err := releaseAsset("1.2.3", tc.goos, tc.goarch)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("releaseAsset: %v", err)
			}
			if got != tc.want {
				t.Fatalf("asset = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestChecksumFor(t *testing.T) {
	checksums := []byte("abc123  ./hive_1.2.3_linux_amd64.tar.gz\r\ndef456  *hive_1.2.3_darwin_arm64.tar.gz\n")

	got, err := checksumFor(checksums, "hive_1.2.3_linux_amd64.tar.gz")
	if err != nil || got != "abc123" {
		t.Fatalf("checksumFor = (%q, %v), want abc123", got, err)
	}
	got, err = checksumFor(checksums, "hive_1.2.3_darwin_arm64.tar.gz")
	if err != nil || got != "def456" {
		t.Fatalf("checksumFor = (%q, %v), want def456", got, err)
	}
	if _, err := checksumFor(checksums, "hive_9.9.9_linux_amd64.tar.gz"); err == nil {
		t.Fatal("expected an error for an asset with no entry")
	}
}

func TestIsCurrent(t *testing.T) {
	for _, tc := range []struct {
		current, target string
		want            bool
	}{
		{"1.1.0", "1.1.0", true},
		{"v1.1.0", "1.1.0", true},
		{"1.0.0", "1.1.0", false},
		{"0.0.0-dev", "1.1.0", false},
		{"", "1.1.0", false},
	} {
		if got := isCurrent(tc.current, tc.target); got != tc.want {
			t.Errorf("isCurrent(%q, %q) = %v, want %v", tc.current, tc.target, got, tc.want)
		}
	}
}

func TestTagFromLocation(t *testing.T) {
	if got := tagFromLocation("https://github.com/thuupx/hive/releases/tag/v1.1.0"); got != "1.1.0" {
		t.Fatalf("tag = %q", got)
	}
	for _, location := range []string{"", "https://github.com/thuupx/hive/releases/latest"} {
		if got := tagFromLocation(location); got != "" {
			t.Errorf("tagFromLocation(%q) = %q, want empty", location, got)
		}
	}
}

// An update replaces hive and its plugins beside the running binary, leaves
// everything else, and verifies the download first.
func TestPerformUpdateInstallsTheRelease(t *testing.T) {
	const version = "9.9.9"

	asset, err := releaseAsset(version, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skipf("no published build for this platform: %v", err)
	}

	tarball := releaseTarball(t, map[string]string{
		"hive":             "new-hive",
		"hive-plugin-zalo": "new-zalo",
	})
	sum := sha256.Sum256(tarball)

	server := httptest.NewServer(releaseServer(t, version, asset, tarball, fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), asset)))
	defer server.Close()

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "hive"), "old-hive")
	writeFile(t, filepath.Join(dir, "hive-plugin-zalo"), "old-zalo")
	writeFile(t, filepath.Join(dir, "unrelated"), "keep")

	var out bytes.Buffer
	changed, err := performUpdate(context.Background(), updateOptions{
		current: "1.0.0",
		target:  version,
		dir:     dir,
		baseURL: server.URL,
		apiURL:  server.URL,
		client:  server.Client(),
		out:     &out,
	})
	if err != nil {
		t.Fatalf("update: %v\n%s", err, out.String())
	}
	if !changed {
		t.Fatal("the update should have changed something")
	}

	if got := readFile(t, filepath.Join(dir, "hive")); got != "new-hive" {
		t.Errorf("hive = %q, want the new binary", got)
	}
	if got := readFile(t, filepath.Join(dir, "hive-plugin-zalo")); got != "new-zalo" {
		t.Errorf("plugin = %q, want the new binary", got)
	}
	if got := readFile(t, filepath.Join(dir, "unrelated")); got != "keep" {
		t.Errorf("an unrelated file was changed: %q", got)
	}

	// No staged copy is left behind beside the real binary.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 3 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("directory holds %v, want no staged files", names)
	}
}

// A download that does not match its checksum is refused, and nothing is
// replaced.
func TestPerformUpdateRefusesABadChecksum(t *testing.T) {
	const version = "9.9.9"

	asset, err := releaseAsset(version, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skipf("no published build for this platform: %v", err)
	}

	tarball := releaseTarball(t, map[string]string{"hive": "new-hive"})
	server := httptest.NewServer(releaseServer(t, version, asset, tarball,
		fmt.Sprintf("%s  %s\n", "deadbeef", asset)))
	defer server.Close()

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "hive"), "old-hive")

	var out bytes.Buffer
	changed, err := performUpdate(context.Background(), updateOptions{
		current: "1.0.0",
		target:  version,
		dir:     dir,
		baseURL: server.URL,
		apiURL:  server.URL,
		client:  server.Client(),
		out:     &out,
	})
	if err == nil {
		t.Fatal("expected a checksum failure")
	}
	if changed {
		t.Fatal("a refused download must not change anything")
	}
	if got := readFile(t, filepath.Join(dir, "hive")); got != "old-hive" {
		t.Errorf("hive = %q, want the old binary", got)
	}
}

// Already current means no download at all.
func TestPerformUpdateIsANoOpWhenCurrent(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "hive"), "old-hive")

	var out bytes.Buffer
	changed, err := performUpdate(context.Background(), updateOptions{
		current: "1.1.0",
		target:  "1.1.0",
		dir:     dir,
		// A base URL that would fail if it were used.
		baseURL: "http://127.0.0.1:0",
		out:     &out,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if changed {
		t.Fatal("nothing should change when the version is current")
	}
	if got := readFile(t, filepath.Join(dir, "hive")); got != "old-hive" {
		t.Errorf("hive = %q, want the old binary", got)
	}
}

// A symlink where a binary should be is refused, not followed.
func TestExtractReleaseRejectsASymlink(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: "hive", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o777,
	}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	tw.Close()
	gz.Close()

	archive := filepath.Join(t.TempDir(), "release.tar.gz")
	if err := os.WriteFile(archive, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}

	if _, err := extractRelease(archive, t.TempDir()); err == nil {
		t.Fatal("a symlink entry should be refused")
	}
}

// The latest version comes from the redirect on /releases/latest.
func TestLatestVersionFromRedirect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/releases/tag/v9.9.9", http.StatusFound)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	got, err := latestVersion(context.Background(), updateOptions{
		baseURL: server.URL,
		apiURL:  server.URL,
		client:  server.Client(),
	})
	if err != nil {
		t.Fatalf("latestVersion: %v", err)
	}
	if got != "9.9.9" {
		t.Fatalf("version = %q, want 9.9.9", got)
	}
}

// Without a redirect, the API is the fallback.
func TestLatestVersionFromAPI(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/base/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "not a redirect")
	})
	mux.HandleFunc("/api/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tag_name":"v9.9.9"}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	got, err := latestVersion(context.Background(), updateOptions{
		baseURL: server.URL + "/base",
		apiURL:  server.URL + "/api",
		client:  server.Client(),
	})
	if err != nil {
		t.Fatalf("latestVersion: %v", err)
	}
	if got != "9.9.9" {
		t.Fatalf("version = %q, want 9.9.9", got)
	}
}

// releaseServer serves one release: its archive and its checksums.
func releaseServer(t *testing.T, version, asset string, tarball []byte, checksums string) *http.ServeMux {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/releases/download/v"+version+"/"+asset, func(w http.ResponseWriter, r *http.Request) {
		w.Write(tarball)
	})
	mux.HandleFunc("/releases/download/v"+version+"/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, checksums)
	})
	return mux
}

// The archive is read as untrusted input: a path that escapes the directory is
// ignored rather than written.
func TestExtractReleaseIgnoresEscapingPaths(t *testing.T) {
	tarball := releaseTarball(t, map[string]string{"../hive": "evil", "hive": "real"})

	archive := filepath.Join(t.TempDir(), "release.tar.gz")
	if err := os.WriteFile(archive, tarball, 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}

	dir := t.TempDir()
	found, err := extractRelease(archive, dir)
	if err != nil {
		t.Fatalf("extractRelease: %v", err)
	}
	if got := readFile(t, found["hive"]); got != "real" {
		t.Fatalf("hive = %q, want the real entry", got)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "hive")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an escaping path must not be written")
	}
}
