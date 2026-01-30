//go:build linux

package source

import (
	"archive/tar"
	"context"
	"crypto/tls"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/google/go-containerregistry/pkg/crane"
	"golang.org/x/sys/unix"

	"github.com/cozystack/boot-to-talos/internal/types"
	"github.com/cozystack/boot-to-talos/internal/uki"
)

// ContainerSource implements ImageSource for container registry images.
type ContainerSource struct {
	ref     string
	tmpDir  string // temporary directory for extracted files
	ukiPath string // path to extracted UKI
}

// NewContainerSource creates a new ContainerSource.
func NewContainerSource(ref string) *ContainerSource {
	return &ContainerSource{ref: ref}
}

func (s *ContainerSource) Type() types.ImageSourceType {
	return types.ImageSourceContainer
}

func (s *ContainerSource) Reference() string {
	return s.ref
}

// setupTransportWithProxy configures HTTP transport with proxy support.
// Clones DefaultTransport to preserve connection pooling, timeouts, and keep-alive settings.
func setupTransportWithProxy() http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	return transport
}

// containerPullTimeout is the maximum time allowed for pulling a container image.
const containerPullTimeout = 30 * time.Minute

// extractUKIFromImage pulls container image and extracts UKI to tmpDir.
//
//nolint:gocognit
func (s *ContainerSource) extractUKIFromImage() (err error) {
	if s.ukiPath != "" {
		return nil // already extracted
	}

	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "container-source-*")
	if err != nil {
		return errors.Wrap(err, "create temp dir")
	}
	s.tmpDir = tmpDir

	// Clean up tmpDir on error (memory leak fix)
	defer func() {
		if err != nil && s.tmpDir != "" {
			os.RemoveAll(s.tmpDir)
			s.tmpDir = ""
			s.ukiPath = ""
		}
	}()

	// Pull image with timeout
	ctx, cancel := context.WithTimeout(context.Background(), containerPullTimeout)
	defer cancel()

	transport := setupTransportWithProxy()
	img, err := crane.Pull(s.ref, crane.WithTransport(transport), crane.WithContext(ctx))
	if err != nil {
		return errors.Wrapf(err, "pull image %s", s.ref)
	}

	// Extract layers looking for UKI
	layers, err := img.Layers()
	if err != nil {
		return errors.Wrap(err, "get layers")
	}

	for _, layer := range layers {
		if err := s.processLayerForUKI(layer); err != nil {
			return err
		}
	}

	if s.ukiPath == "" {
		return errors.New("UKI kernel (vmlinuz.efi) not found in image")
	}

	return nil
}

// processLayerForUKI processes a single layer looking for UKI file.
// Using a separate function ensures defer r.Close() executes after each layer.
func (s *ContainerSource) processLayerForUKI(layer interface{ Uncompressed() (io.ReadCloser, error) }) error {
	r, err := layer.Uncompressed()
	if err != nil {
		return errors.Wrap(err, "uncompress layer")
	}
	defer r.Close()

	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return errors.Wrap(err, "read tar")
		}

		// Skip whiteout files
		if strings.HasPrefix(filepath.Base(header.Name), ".wh.") {
			continue
		}

		// Look for UKI kernel
		name := strings.ToLower(header.Name)
		if strings.Contains(name, "install") && strings.Contains(name, "vmlinuz.efi") {
			target := filepath.Join(s.tmpDir, filepath.Base(header.Name))
			f, err := os.Create(target)
			if err != nil {
				return errors.Wrap(err, "create UKI file")
			}

			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return errors.Wrap(err, "extract UKI")
			}
			f.Close()

			s.ukiPath = target
			return nil // Found UKI, stop processing
		}
	}
}

// GetBootAssets extracts kernel and initrd from UKI in container image.
func (s *ContainerSource) GetBootAssets() (*types.BootAssets, error) {
	if err := s.extractUKIFromImage(); err != nil {
		return nil, err
	}

	// Extract UKI sections
	ukiAssets, err := uki.Extract(s.ukiPath)
	if err != nil {
		return nil, errors.Wrap(err, "extract UKI")
	}

	// Read cmdline
	cmdlineBytes, err := io.ReadAll(ukiAssets.Cmdline)
	if err != nil {
		ukiAssets.Close()
		return nil, errors.Wrap(err, "read cmdline")
	}
	cmdline := strings.TrimRight(string(cmdlineBytes), "\x00")
	cmdline = strings.TrimSpace(cmdline)

	// Create BootAssets with readers
	// Note: We need to reopen the UKI to get fresh readers since ukiAssets.Cmdline was consumed
	ukiAssets.Close()

	ukiAssets2, err := uki.Extract(s.ukiPath)
	if err != nil {
		return nil, errors.Wrap(err, "extract UKI for readers")
	}

	// Create shared closer to prevent double-close when both Kernel and Initrd
	// are closed (they share the same underlying PE file)
	shared := &sharedCloser{closer: ukiAssets2}

	return &types.BootAssets{
		Kernel:  &readerCloser{reader: ukiAssets2.Kernel, closer: shared},
		Initrd:  &readerCloser{reader: ukiAssets2.Initrd, closer: shared},
		Cmdline: cmdline,
	}, nil
}

// GetInstallAssets extracts the full rootfs for chroot installation.
func (s *ContainerSource) GetInstallAssets(tmpDir string, _ uint64) (*types.InstallAssets, error) {
	// Pull image with timeout
	ctx, cancel := context.WithTimeout(context.Background(), containerPullTimeout)
	defer cancel()

	transport := setupTransportWithProxy()
	img, err := crane.Pull(s.ref, crane.WithTransport(transport), crane.WithContext(ctx))
	if err != nil {
		return nil, errors.Wrapf(err, "pull image %s", s.ref)
	}

	// Get layers
	layers, err := img.Layers()
	if err != nil {
		return nil, errors.Wrap(err, "get layers")
	}

	// Create destination directory for rootfs
	rootfsDir := filepath.Join(tmpDir, "rootfs")
	if err := os.MkdirAll(rootfsDir, 0o755); err != nil {
		return nil, errors.Wrap(err, "create rootfs directory")
	}

	// Extract all layers to rootfs directory
	for _, layer := range layers {
		if err := extractLayer(layer, rootfsDir); err != nil {
			return nil, errors.Wrap(err, "extract layer")
		}
	}

	return &types.InstallAssets{
		RootfsPath: rootfsDir,
		Cleanup: func() error {
			return os.RemoveAll(rootfsDir)
		},
	}, nil
}

// extractLayer extracts a single container layer to destDir.
//
//nolint:gocognit
func extractLayer(layer interface{ Uncompressed() (io.ReadCloser, error) }, destDir string) error {
	r, err := layer.Uncompressed()
	if err != nil {
		return errors.Wrap(err, "uncompress layer")
	}
	defer r.Close()

	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return errors.Wrap(err, "read tar")
		}

		// Handle whiteout files (OCI layer deletions)
		if suffix, found := strings.CutPrefix(filepath.Base(header.Name), ".wh."); found {
			_ = os.RemoveAll(filepath.Join(destDir, filepath.Dir(header.Name), suffix))
			continue
		}

		target := filepath.Join(destDir, header.Name)

		// Security: prevent path traversal attacks
		cleanTarget := filepath.Clean(target)
		cleanDest := filepath.Clean(destDir)
		if !strings.HasPrefix(cleanTarget, cleanDest+string(os.PathSeparator)) && cleanTarget != cleanDest {
			continue // skip files that would escape destDir
		}

		switch header.Typeflag {
		case tar.TypeDir:
			_ = os.MkdirAll(target, os.FileMode(header.Mode))
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return errors.Wrap(err, "create directory")
			}
			f, err := os.Create(target)
			if err != nil {
				return errors.Wrap(err, "create file")
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return errors.Wrap(err, "extract file")
			}
			f.Close()
			_ = os.Chmod(target, os.FileMode(header.Mode))
		case tar.TypeSymlink:
			// Validate symlink target doesn't escape destDir
			linkTarget := header.Linkname
			if !filepath.IsAbs(linkTarget) {
				linkTarget = filepath.Join(filepath.Dir(target), linkTarget)
			}
			cleanLink := filepath.Clean(linkTarget)
			if !strings.HasPrefix(cleanLink, cleanDest+string(os.PathSeparator)) && cleanLink != cleanDest {
				log.Printf("warning: skipping symlink escape: %s -> %s", target, header.Linkname)
				continue
			}
			_ = os.MkdirAll(filepath.Dir(target), 0o755)
			_ = os.Remove(target) // Remove existing symlink if any
			if err := os.Symlink(header.Linkname, target); err != nil && !os.IsExist(err) {
				log.Printf("warning: symlink %s -> %s: %v", target, header.Linkname, err)
			}
		case tar.TypeLink:
			// Validate hardlink source doesn't escape destDir
			linkSource := filepath.Join(destDir, header.Linkname)
			cleanSource := filepath.Clean(linkSource)
			if !strings.HasPrefix(cleanSource, cleanDest+string(os.PathSeparator)) && cleanSource != cleanDest {
				log.Printf("warning: skipping hardlink escape: %s -> %s", target, header.Linkname)
				continue
			}
			_ = os.MkdirAll(filepath.Dir(target), 0o755)
			if err := os.Link(linkSource, target); err != nil && !os.IsExist(err) {
				log.Printf("warning: hardlink %s -> %s: %v", target, header.Linkname, err)
			}
		case tar.TypeChar, tar.TypeBlock:
			_ = os.MkdirAll(filepath.Dir(target), 0o755)
			dev := int(unix.Mkdev(uint32(header.Devmajor), uint32(header.Devminor)))
			mode := uint32(header.Mode)
			if header.Typeflag == tar.TypeChar {
				mode |= unix.S_IFCHR
			} else {
				mode |= unix.S_IFBLK
			}
			_ = unix.Mknod(target, mode, dev)
		}
	}
	return nil
}

func (s *ContainerSource) Close() error {
	if s.tmpDir != "" {
		os.RemoveAll(s.tmpDir)
		s.tmpDir = ""
	}
	return nil
}
