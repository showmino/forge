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
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/exitcode"
)

// maxArchive caps a downloaded CLI archive.
const maxArchive = 100 << 20

var releaseVersion = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

func newUpgradeCmd(f *Factory) *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade forge to the Forge API's version",
		Long: `Upgrade forge to the version of the Forge API it talks to, for this
operating system: the API serves the CLI archives of its release, so no
GitHub access is needed. The download is checked against the release's
checksums before the forge binary is replaced.`,
		Example: `  forge upgrade --check
  forge upgrade`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
			if !releaseVersion.MatchString(want) {
				return exitcode.New(exitcode.Unexpected, "the Forge API runs %q, not a release: there is nothing to download", want)
			}
			if want == f.Version {
				_, err := fmt.Fprintf(f.IO.Out, "forge %s is the server's version: nothing to do\n", want)
				return err
			}
			if check {
				_, err := fmt.Fprintf(f.IO.Out, "forge %s is available (this is %s); run 'forge upgrade'\n", want, f.Version)
				return err
			}
			exe, err := f.Executable()
			if err != nil {
				return fmt.Errorf("find this forge binary: %w", err)
			}
			if resolved, err := filepath.EvalSymlinks(exe); err == nil {
				exe = resolved
			}
			bin, err := f.downloadCLI(ctx, want)
			if err != nil {
				return err
			}
			if err := replaceBinary(exe, bin); err != nil {
				if errors.Is(err, fs.ErrPermission) {
					return exitcode.New(exitcode.PermissionDenied, "replace %s: %v", exe, err).
						WithHint("run 'forge upgrade' as a user that may write there (for example with sudo)")
				}
				return fmt.Errorf("replace %s: %w", exe, err)
			}
			_, err = fmt.Fprintf(f.IO.Out, "forge upgraded from %s to %s (%s)\n", f.Version, want, exe)
			return err
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "only say whether there is another version")
	return cmd
}

// downloadCLI fetches this platform's archive of release version from the
// API, checks it against the release's checksums and returns the binary.
func (f *Factory) downloadCLI(ctx context.Context, version string) ([]byte, error) {
	r, err := f.Resolved()
	if err != nil {
		return nil, err
	}
	base := strings.TrimRight(r.APIURL, "/") + "/cli/releases/v" + version + "/"
	archive := fmt.Sprintf("forge_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		archive = strings.TrimSuffix(archive, ".tar.gz") + ".zip"
	}
	sums, err := f.fetch(ctx, base+"checksums.txt")
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
	data, err := f.fetch(ctx, base+archive)
	if err != nil {
		return nil, err
	}
	if got := sha256.Sum256(data); hex.EncodeToString(got[:]) != want {
		return nil, exitcode.New(exitcode.Unexpected, "%s does not match its checksum in release %s; nothing was replaced", archive, version)
	}
	return extractBinary(archive, data)
}

func (f *Factory) fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", path.Base(url), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, exitcode.New(exitcode.NotFound, "the Forge API does not serve %s", path.Base(url)).
			WithHint("the API may be older than 'forge upgrade', or the release has no build for this system")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", path.Base(url), resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxArchive+1))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", path.Base(url), err)
	}
	if len(data) > maxArchive {
		return nil, fmt.Errorf("download %s: larger than %d bytes", path.Base(url), maxArchive)
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
				return io.ReadAll(io.LimitReader(rc, maxArchive))
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
			return io.ReadAll(io.LimitReader(tr, maxArchive))
		}
	}
}

// replaceBinary puts bin in place of exe. Windows cannot overwrite a running
// program, but it can rename it: the old one is set aside as exe.old.
func replaceBinary(exe string, bin []byte) error {
	tmp := exe + ".new"
	if err := os.WriteFile(tmp, bin, 0o755); err != nil { //nolint:gosec // an executable must be executable
		return err
	}
	if runtime.GOOS == "windows" {
		old := exe + ".old"
		_ = os.Remove(old)
		if err := os.Rename(exe, old); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		if err := os.Rename(tmp, exe); err != nil {
			_ = os.Rename(old, exe)
			return err
		}
		return nil
	}
	if err := os.Rename(tmp, exe); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
