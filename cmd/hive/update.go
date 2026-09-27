package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// The release source. It mirrors scripts/install.sh, so a self-update and a fresh
// install land the same binaries in the same shape.
const (
	defaultReleaseBase = "https://github.com/thuupx/hive"
	defaultReleaseAPI  = "https://api.github.com/repos/thuupx/hive"

	// maxArchiveBytes bounds a download, so a wrong URL cannot fill the disk.
	maxArchiveBytes = 256 << 20

	// maxChecksumsBytes bounds the checksums file, which is a few hundred bytes.
	maxChecksumsBytes = 1 << 20

	// updateUserAgent identifies the client to GitHub. The API rejects a request
	// without one.
	updateUserAgent = "hive-update"
)

// releaseBinaries are the files a release archive holds, beside hive.
var releaseBinaries = []string{"hive", "hive-plugin-acp", "hive-plugin-slack", "hive-plugin-zalo"}

// runUpdate replaces this installation with a release.
//
// It downloads the tarball for this platform, verifies its SHA-256 against the
// release's checksums.txt, and replaces the binaries next to the running one —
// which is where the daemon resolves the plugins, so both move together.
func runUpdate(f flags, args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	version := fs.String("version", "", "the release to install (default: the latest)")
	dir := fs.String("dir", "", "where to install (default: the running binary's directory)")
	force := fs.Bool("force", false, "install even when the version is already current")
	if err := parseArgsAndFlags(fs, args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	targetDir := *dir
	if targetDir == "" {
		self, err := os.Executable()
		if err != nil {
			return fmt.Errorf("resolve the running binary: %w", err)
		}
		// Plugins ship next to hive and the daemon resolves them from there, so
		// the directory to update is the one the running binary lives in.
		targetDir = filepath.Dir(resolvePath(self))
	}

	changed, err := performUpdate(ctx, updateOptions{
		current: Version,
		target:  strings.TrimPrefix(*version, "v"),
		dir:     targetDir,
		force:   *force,
		baseURL: defaultReleaseBase,
		apiURL:  defaultReleaseAPI,
		client:  &http.Client{Timeout: 10 * time.Minute},
		out:     os.Stdout,
	})
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}

	// A running daemon keeps the binary it was started from, so replacing the
	// file does not replace the process: an update is finished with a restart.
	if serviceInstalled() {
		fmt.Println("hive: restarting the service so it runs the new build")
		return restartService(f)
	}
	fmt.Println("hive: run `hive serve` to run the new build")
	return nil
}

// updateOptions is one update, resolved.
type updateOptions struct {
	// current is the running build's version.
	current string

	// target is the release to install, without the leading "v". Empty means the
	// latest release.
	target string

	// dir is where the binaries are installed.
	dir string

	// force installs even when target is already current.
	force bool

	baseURL string
	apiURL  string
	client  *http.Client
	out     io.Writer
}

// performUpdate installs the target release, reporting whether it changed
// anything. It is separate from runUpdate so it can be tested without a service.
func performUpdate(ctx context.Context, opts updateOptions) (bool, error) {
	if opts.out == nil {
		opts.out = io.Discard
	}
	if opts.client == nil {
		opts.client = &http.Client{Timeout: 10 * time.Minute}
	}

	version := opts.target
	if version == "" {
		latest, err := latestVersion(ctx, opts)
		if err != nil {
			return false, err
		}
		version = latest
	}

	if !opts.force && isCurrent(opts.current, version) {
		fmt.Fprintf(opts.out, "hive: %s is already the current version\n", version)
		return false, nil
	}

	asset, err := releaseAsset(version, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return false, err
	}
	fmt.Fprintf(opts.out, "hive: %s -> %s\n", displayVersion(opts.current), version)
	fmt.Fprintf(opts.out, "hive: downloading %s\n", asset)

	tmp, err := os.MkdirTemp("", "hive-update-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(tmp)

	base := strings.TrimRight(opts.baseURL, "/") + "/releases/download/v" + version
	archive := filepath.Join(tmp, asset)
	if err := download(ctx, opts.client, base+"/"+asset, archive); err != nil {
		return false, err
	}
	checksums, err := fetch(ctx, opts.client, base+"/checksums.txt", maxChecksumsBytes)
	if err != nil {
		return false, err
	}

	// The checksum is verified before the archive is opened: an archive that is
	// never extracted cannot do anything, whatever is in it.
	if err := verifyChecksum(archive, asset, checksums); err != nil {
		return false, err
	}
	fmt.Fprintln(opts.out, "hive: verified the sha256 checksum")

	extractDir := filepath.Join(tmp, "extract")
	if err := os.MkdirAll(extractDir, 0o700); err != nil {
		return false, err
	}
	extracted, err := extractRelease(archive, extractDir)
	if err != nil {
		return false, err
	}
	if _, ok := extracted["hive"]; !ok {
		return false, fmt.Errorf("%s does not contain hive", asset)
	}

	if err := os.MkdirAll(opts.dir, 0o700); err != nil {
		return false, err
	}
	installed := make([]string, 0, len(releaseBinaries))
	for _, name := range releaseBinaries {
		source, ok := extracted[name]
		if !ok {
			// An older release may carry fewer plugins; installing the ones it
			// has is better than refusing the whole update.
			continue
		}
		if err := replaceBinary(source, opts.dir, name); err != nil {
			return false, err
		}
		installed = append(installed, name)
	}
	fmt.Fprintf(opts.out, "hive: installed %s to %s\n", strings.Join(installed, ", "), opts.dir)
	return true, nil
}

// releaseAsset is the archive name for a platform, or an error for one with no
// published build.
func releaseAsset(version, goos, goarch string) (string, error) {
	switch goos {
	case "linux", "darwin":
	default:
		return "", fmt.Errorf("%s is not supported; Hive releases cover linux and darwin", goos)
	}
	switch goarch {
	case "amd64", "arm64":
	default:
		return "", fmt.Errorf("%s is not supported; Hive releases cover amd64 and arm64", goarch)
	}
	if goos == "darwin" && goarch == "amd64" {
		return "", errors.New("darwin/amd64 is not published; build from source with `make build`")
	}
	return fmt.Sprintf("hive_%s_%s_%s.tar.gz", version, goos, goarch), nil
}

// isCurrent reports whether the running build is the target version.
//
// A development build reports a version that is not a release, so it is never
// current: updating it to a release is the point.
func isCurrent(current, target string) bool {
	if current == "" || strings.Contains(current, "dev") {
		return false
	}
	return strings.TrimPrefix(current, "v") == target
}

// displayVersion renders a running version for the log.
func displayVersion(current string) string {
	if current == "" || strings.Contains(current, "dev") {
		return "development build"
	}
	return current
}

// latestVersion resolves the newest release tag, without its leading "v".
//
// The redirect on /releases/latest names the tag, and it is one request that is
// not rate-limited per address the way the API is. The API is the fallback.
func latestVersion(ctx context.Context, opts updateOptions) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(opts.baseURL, "/")+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", updateUserAgent)

	client := *opts.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if resp, err := client.Do(req); err == nil {
		resp.Body.Close()
		if tag := tagFromLocation(resp.Header.Get("Location")); tag != "" {
			return tag, nil
		}
	}

	body, err := fetch(ctx, opts.client, strings.TrimRight(opts.apiURL, "/")+"/releases/latest", maxChecksumsBytes)
	if err != nil {
		return "", fmt.Errorf("resolve the latest release: %w", err)
	}
	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &release); err != nil {
		return "", fmt.Errorf("resolve the latest release: %w", err)
	}
	if release.TagName == "" {
		return "", errors.New("could not resolve the latest release; pass -version")
	}
	return strings.TrimPrefix(release.TagName, "v"), nil
}

// tagFromLocation reads a release tag from a redirect's Location header.
func tagFromLocation(location string) string {
	if location == "" {
		return ""
	}
	base := path.Base(location)
	if base == "" || base == "." || base == "latest" {
		return ""
	}
	return strings.TrimPrefix(base, "v")
}

// fetch reads a small URL whole.
func fetch(ctx context.Context, client *http.Client, url string, limit int64) ([]byte, error) {
	resp, err := get(ctx, client, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: %s", url, resp.Status)
	}
	body, err := readLimited(resp.Body, limit)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	return body, nil
}

// download streams a URL to a file.
func download(ctx context.Context, client *http.Client, url, target string) error {
	resp, err := get(ctx, client, url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", url, resp.Status)
	}

	file, err := os.Create(target)
	if err != nil {
		return err
	}
	defer file.Close()

	if _, err := io.Copy(file, io.LimitReader(resp.Body, maxArchiveBytes+1)); err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	if info, err := file.Stat(); err == nil && info.Size() > maxArchiveBytes {
		return fmt.Errorf("download %s: larger than %d bytes", url, maxArchiveBytes)
	}
	return nil
}

// get performs a GET with the headers GitHub expects.
func get(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", updateUserAgent)
	req.Header.Set("Accept", "application/vnd.github+json")
	return client.Do(req)
}

// readLimited reads at most limit bytes, reporting a body that is larger.
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("response is larger than %d bytes", limit)
	}
	return body, nil
}

// verifyChecksum checks an archive against a release's checksums.txt.
func verifyChecksum(archive, asset string, checksums []byte) error {
	expected, err := checksumFor(checksums, asset)
	if err != nil {
		return err
	}

	file, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	actual := hex.EncodeToString(hash.Sum(nil))

	if !strings.EqualFold(expected, actual) {
		return fmt.Errorf("checksum mismatch for %s\n  expected %s\n  actual   %s\nRefusing to install a download that does not match its checksum.",
			asset, expected, actual)
	}
	return nil
}

// checksumFor reads the digest for one asset from checksums.txt.
//
// The name is compared without the "./" a shell glob may have left on it, without
// the "*" a checksum tool may prefix in binary mode, and without a carriage return
// a file written on Windows would leave on it.
func checksumFor(checksums []byte, asset string) (string, error) {
	for _, line := range strings.Split(string(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		name := strings.TrimSuffix(fields[1], "\r")
		name = strings.TrimPrefix(name, "*")
		name = strings.TrimPrefix(name, "./")
		if name == asset {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("checksums.txt has no entry for %s", asset)
}

// extractRelease writes the release's binaries out of the archive.
//
// Only the named files are read, and only as regular files: an archive is
// untrusted input, so a symlink or a path that escapes the directory is refused
// rather than followed.
func extractRelease(archive, dir string) (map[string]string, error) {
	file, err := os.Open(archive)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	gz, err := gzip.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(archive), err)
	}
	defer gz.Close()

	wanted := make(map[string]bool, len(releaseBinaries))
	for _, name := range releaseBinaries {
		wanted[name] = true
	}

	found := map[string]string{}
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", filepath.Base(archive), err)
		}

		name := path.Clean(header.Name)
		if !wanted[name] {
			continue
		}
		if header.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("%s is not a regular file", name)
		}

		target := filepath.Join(dir, name)
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return nil, err
		}
		if _, err := io.Copy(out, io.LimitReader(reader, maxArchiveBytes)); err != nil {
			out.Close()
			return nil, err
		}
		if err := out.Close(); err != nil {
			return nil, err
		}
		found[name] = target
	}
	return found, nil
}

// replaceBinary installs one binary over its target.
//
// It is staged beside the target and renamed, because a rename is atomic within
// one filesystem: writing over a binary the daemon is running does not work, and
// Linux refuses it with "Text file busy".
func replaceBinary(source, dir, name string) error {
	target := filepath.Join(dir, name)

	staged, err := os.CreateTemp(dir, "."+name+".new.*")
	if err != nil {
		return err
	}
	stagedPath := staged.Name()
	defer os.Remove(stagedPath)

	in, err := os.Open(source)
	if err != nil {
		staged.Close()
		return err
	}
	defer in.Close()

	if _, err := io.Copy(staged, in); err != nil {
		staged.Close()
		return err
	}
	if err := staged.Chmod(0o755); err != nil {
		staged.Close()
		return err
	}
	if err := staged.Close(); err != nil {
		return err
	}
	return os.Rename(stagedPath, target)
}
