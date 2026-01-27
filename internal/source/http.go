package source

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/cockroachdb/errors"

	"github.com/cozystack/boot-to-talos/internal/types"
)

// downloadTimeout is the maximum time allowed for downloading an image.
const downloadTimeout = 30 * time.Minute

// ProgressFunc is called during download to report progress.
type ProgressFunc func(current, total int64)

// progressReader wraps an io.Reader and reports progress.
type progressReader struct {
	reader     io.Reader
	total      int64
	current    int64
	onProgress ProgressFunc
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.reader.Read(p)
	if n > 0 {
		pr.current += int64(n)
		if pr.onProgress != nil {
			pr.onProgress(pr.current, pr.total)
		}
	}
	return n, err
}

// DownloadToFile downloads a URL to a local file with optional progress reporting.
func DownloadToFile(ctx context.Context, url, destPath string, onProgress ProgressFunc) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return errors.Wrap(err, "create request")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return errors.Wrapf(err, "download %s", url)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return errors.Newf("download %s: HTTP %d %s", url, resp.StatusCode, resp.Status)
	}

	// Validate Content-Type to catch error pages served as 200 OK
	contentType := resp.Header.Get("Content-Type")
	if contentType != "" && strings.HasPrefix(contentType, "text/html") {
		return errors.Newf("download %s: unexpected Content-Type %s (server may have returned error page)", url, contentType)
	}

	// Create destination file
	file, err := os.Create(destPath)
	if err != nil {
		return errors.Wrapf(err, "create file %s", destPath)
	}
	defer file.Close()

	// Wrap reader with progress reporting
	var reader io.Reader = resp.Body
	if onProgress != nil {
		reader = &progressReader{
			reader:     resp.Body,
			total:      resp.ContentLength,
			onProgress: onProgress,
		}
	}

	// Copy data
	_, err = io.Copy(file, reader)
	if err != nil {
		return errors.Wrapf(err, "write file %s", destPath)
	}

	return nil
}

// HTTPSource wraps a remote image that needs to be downloaded first.
type HTTPSource struct {
	url             string
	targetType      types.ImageSourceType
	tempFile        string            // path to downloaded file
	delegatedSource types.ImageSource // source created for delegation
}

// NewHTTPSource creates a new HTTPSource for downloading remote images.
func NewHTTPSource(url string, targetType types.ImageSourceType) *HTTPSource {
	return &HTTPSource{
		url:        url,
		targetType: targetType,
	}
}

func (s *HTTPSource) Type() types.ImageSourceType {
	return s.targetType
}

func (s *HTTPSource) Reference() string {
	return s.url
}

// GetBootAssets downloads the image and delegates to appropriate source.
func (s *HTTPSource) GetBootAssets() (*types.BootAssets, error) {
	// Download to temp file
	if err := s.ensureDownloaded(); err != nil {
		return nil, err
	}

	// Delegate to appropriate source based on type
	switch s.targetType {
	case types.ImageSourceRAW:
		rawSource := NewRAWSource(s.tempFile)
		s.delegatedSource = rawSource
		return rawSource.GetBootAssets()
	case types.ImageSourceISO:
		isoSource := NewISOSource(s.tempFile)
		s.delegatedSource = isoSource
		return isoSource.GetBootAssets()
	case types.ImageSourceContainer:
		return nil, errors.New("HTTP source cannot handle container images - use container source directly")
	}
	return nil, errors.Newf("unsupported source type: %v", s.targetType)
}

// GetInstallAssets downloads the image and delegates to appropriate source.
func (s *HTTPSource) GetInstallAssets(tmpDir string, sizeGiB uint64) (*types.InstallAssets, error) {
	// Download to temp file
	if err := s.ensureDownloaded(); err != nil {
		return nil, err
	}

	// Delegate to appropriate source based on type
	switch s.targetType {
	case types.ImageSourceRAW:
		rawSource := NewRAWSource(s.tempFile)
		s.delegatedSource = rawSource
		return rawSource.GetInstallAssets(tmpDir, sizeGiB)
	case types.ImageSourceISO:
		return nil, errors.New("ISO source install mode not supported")
	case types.ImageSourceContainer:
		return nil, errors.New("HTTP source cannot handle container images - use container source directly")
	}
	return nil, errors.Newf("unsupported source type: %v", s.targetType)
}

// ensureDownloaded downloads the file to temp if not already downloaded.
func (s *HTTPSource) ensureDownloaded() error {
	if s.tempFile != "" {
		return nil // already downloaded
	}

	// Create temp file
	tmpFile, err := os.CreateTemp("", "http-source-*")
	if err != nil {
		return errors.Wrap(err, "create temp file")
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()

	// Download with timeout
	ctx, cancel := context.WithTimeout(context.Background(), downloadTimeout)
	defer cancel()
	if err := DownloadToFile(ctx, s.url, tmpPath, nil); err != nil {
		os.Remove(tmpPath)
		return errors.Wrap(err, "download")
	}

	s.tempFile = tmpPath
	return nil
}

func (s *HTTPSource) Close() error {
	var errs []error

	// Close delegated source first
	if s.delegatedSource != nil {
		if err := s.delegatedSource.Close(); err != nil {
			errs = append(errs, err)
		}
		s.delegatedSource = nil
	}

	// Clean up downloaded temp file if any
	if s.tempFile != "" {
		if err := os.Remove(s.tempFile); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
		s.tempFile = ""
	}

	return errors.Join(errs...)
}
