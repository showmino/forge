package cli

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/semver"
)

// maxArchive caps a downloaded CLI archive and the binary in it; maxChecksums
// caps checksums.txt.
const (
	maxArchive   = 100 << 20
	maxChecksums = 1 << 20
)

var releaseVersion = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

func newUpgradeCmd(f *Factory) *cobra.Command {
	var check, allowDowngrade bool
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade forge to the Forge API's version",
		Long: `Upgrade forge to the version of the Forge API it talks to, for this
operating system: the API serves the CLI archives of its release, so no
GitHub access is needed. The download is checked against the release's
checksums before the forge binary is replaced. The API must be reached over
HTTPS (or on this machine), because it decides what is installed. An older
version than this one is only installed with --allow-downgrade.`,
		Example: `  forge upgrade --check
  forge upgrade`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := f.Resolved()
			if err != nil {
				return err
			}
			if err := trustedAPI(r.APIURL); err != nil {
				return err
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, false)
			if err != nil {
				return err
			}
			info, err := c.Auth.GetServerInfo(ctx, connect.NewRequest(&forgev1.GetServerInfoRequest{}))
			if err != nil {
				return err
			}
			want := info.Msg.GetServerVersion()
			serverV, ok := semver.Parse("v" + want)
			if !releaseVersion.MatchString(want) || !ok {
				return exitcode.New(exitcode.Unexpected, "the Forge API runs %q, not a release: there is nothing to download", want)
			}
			if want == f.Version {
				_, err := fmt.Fprintf(f.IO.Out, "forge %s is the server's version: nothing to do\n", want)
				return err
			}
			mine, ok := semver.Parse("v" + f.Version)
			if !ok {
				return exitcode.Usagef("this forge is a development build (%s); it is not replaced", f.Version)
			}
			older := semver.Compare(serverV, mine) < 0
			if check {
				word := "is available"
				if older {
					word = "is the server's version, older than this one"
				}
				_, err := fmt.Fprintf(f.IO.Out, "forge %s %s (this is %s); run 'forge upgrade'\n", want, word, f.Version)
				return err
			}
			if older && !allowDowngrade {
				return exitcode.Usagef("the Forge API runs %s, older than this forge (%s)", want, f.Version).
					WithHint("pass --allow-downgrade to install it anyway")
			}
			exe, err := f.Executable()
			if err != nil {
				return fmt.Errorf("find this forge binary: %w", err)
			}
			if resolved, err := filepath.EvalSymlinks(exe); err == nil {
				exe = resolved
			}
			if strings.Contains(filepath.ToSlash(exe), "/Caskroom/") || strings.Contains(filepath.ToSlash(exe), "/Cellar/") {
				return exitcode.Usagef("%s is managed by Homebrew", exe).WithHint("run 'brew upgrade forge'")
			}
			bin, err := f.downloadCLI(ctx, r.APIURL, want)
			if err != nil {
				return err
			}
			if err := replaceBinary(exe, bin); err != nil {
				if errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EROFS) {
					return exitcode.New(exitcode.PermissionDenied, "replace %s: %v", exe, err).
						WithHint(fmt.Sprintf("run it as a user that may write there, naming this API: sudo forge upgrade --api-url %s", r.APIURL))
				}
				return fmt.Errorf("replace %s: %w", exe, err)
			}
			_, err = fmt.Fprintf(f.IO.Out, "forge upgraded from %s to %s (%s)\n", f.Version, want, exe)
			return err
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "only say whether there is another version")
	cmd.Flags().BoolVar(&allowDowngrade, "allow-downgrade", false, "install the API's version also when it is older")
	return cmd
}

// trustedAPI refuses an API reached in plain HTTP on the network: whoever
// answers there would choose the binary.
func trustedAPI(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return exitcode.Usagef("API URL %q: %v", raw, err)
	}
	if u.Scheme == "https" {
		return nil
	}
	if ip := net.ParseIP(u.Hostname()); u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback()) {
		return nil
	}
	return exitcode.Usagef("forge upgrade needs the API over HTTPS, not %s", raw).
		WithHint("name the HTTPS address, for example --api-url https://forge.example.com")
}

// downloadCLI fetches this platform's archive of release version from the
// API, checks it against the release's checksums and returns the binary.
func (f *Factory) downloadCLI(ctx context.Context, apiURL, version string) ([]byte, error) {
	base := strings.TrimRight(apiURL, "/") + "/cli/releases/v" + version + "/"
	archive := fmt.Sprintf("forge_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		archive = strings.TrimSuffix(archive, ".tar.gz") + ".zip"
	}
	sums, err := f.fetch(ctx, base+"checksums.txt", maxChecksums)
	if err != nil {
		return nil, err
	}
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		if sum, name, ok := strings.Cut(strings.TrimSpace(line), "  "); ok && name == archive {
			want = sum
		}
	}
	if want == "" {
		return nil, exitcode.New(exitcode.NotFound, "release %s has no %s: no forge build for %s/%s", version, archive, runtime.GOOS, runtime.GOARCH)
	}
	data, err := f.fetch(ctx, base+archive, maxArchive)
	if err != nil {
		return nil, err
	}
	if got := sha256.Sum256(data); hex.EncodeToString(got[:]) != want {
		return nil, exitcode.New(exitcode.Unexpected, "%s does not match its checksum in release %s; nothing was replaced", archive, version)
	}
	return extractBinary(archive, data)
}

// fetch downloads addr, bounded by ctx (--timeout) rather than the HTTP
// client's own timeout, which would cut a slow download short.
func (f *Factory) fetch(ctx context.Context, addr string, limit int64) ([]byte, error) {
	name := path.Base(addr)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr, nil)
	if err != nil {
		return nil, err
	}
	resp, err := withoutTimeout(f.HTTPClient).Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, exitcode.New(exitcode.NotFound, "the Forge API does not serve %s", name).
			WithHint("the API may be older than 'forge upgrade', or the release has no build for this system")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", name, resp.Status)
	}
	data, err := readLimited(resp.Body, limit)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", name, err)
	}
	return data, nil
}

// readLimited reads r whole, failing instead of truncating past limit.
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("larger than %d bytes", limit)
	}
	return data, nil
}

// extractBinary returns forge (forge.exe on Windows) from a release archive.
func extractBinary(archive string, data []byte) ([]byte, error) {
	if strings.HasSuffix(archive, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", archive, err)
		}
		for _, zf := range zr.File {
			if zf.Name == "forge.exe" {
				rc, err := zf.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return readLimited(rc, maxArchive)
			}
		}
		return nil, fmt.Errorf("%s has no forge.exe", archive)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", archive, err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s has no forge binary", archive)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", archive, err)
		}
		if h.Name == "forge" && h.Typeflag == tar.TypeReg {
			return readLimited(tr, maxArchive)
		}
	}
}

// replaceBinary puts bin in place of exe, keeping exe's mode. It writes a
// temporary file next to exe and renames it over; Windows cannot overwrite a
// running program, but it can rename it: the old one is set aside as exe.old.
func replaceBinary(exe string, bin []byte) (err error) {
	info, err := os.Stat(exe)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".forge-upgrade-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	_, err = tmp.Write(bin)
	if err == nil {
		err = tmp.Sync()
	}
	if err == nil {
		err = tmp.Chmod(info.Mode().Perm() | 0o100)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		old := exe + ".old"
		_ = os.Remove(old)
		if err = os.Rename(exe, old); err != nil {
			return err
		}
		if err = os.Rename(tmp.Name(), exe); err != nil {
			_ = os.Rename(old, exe)
			return err
		}
		return nil
	}
	return os.Rename(tmp.Name(), exe)
}
