package source

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/cockroachdb/errors"

	"github.com/cozystack/boot-to-talos/internal/types"
)

// DetectImageSource detects the image type and returns an appropriate ImageSource.
func DetectImageSource(ref string) (types.ImageSource, error) {
	// Check if it's a URL
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return detectHTTPImageSource(ref)
	}

	// Check if it's a local file
	if _, err := os.Stat(ref); err == nil {
		return detectLocalImageSource(ref)
	}

	// Assume it's a container reference
	return NewContainerSource(ref), nil
}

// detectHTTPImageSource detects image type from HTTP URL.
func detectHTTPImageSource(rawURL string) (types.ImageSource, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, errors.Wrap(err, "invalid URL")
	}

	path := strings.ToLower(u.Path)

	switch {
	case strings.HasSuffix(path, ".iso"):
		return NewHTTPSource(rawURL, types.ImageSourceISO), nil
	case strings.HasSuffix(path, ".raw.xz"),
		strings.HasSuffix(path, ".raw.gz"),
		strings.HasSuffix(path, ".raw.zst"),
		strings.HasSuffix(path, ".raw"):
		return NewHTTPSource(rawURL, types.ImageSourceRAW), nil
	default:
		// Assume container reference for URLs without recognized extension
		return NewContainerSource(rawURL), nil
	}
}

// detectLocalImageSource detects image type from local file.
func detectLocalImageSource(path string) (types.ImageSource, error) {
	lower := strings.ToLower(path)
	base := strings.ToLower(filepath.Base(path))

	switch {
	case strings.HasSuffix(lower, ".iso"):
		return NewISOSource(path), nil
	case strings.HasSuffix(lower, ".raw.xz"),
		strings.HasSuffix(lower, ".raw.gz"),
		strings.HasSuffix(lower, ".raw.zst"):
		return NewRAWSource(path), nil
	case strings.HasSuffix(lower, ".raw"):
		return NewRAWSource(path), nil
	case strings.Contains(base, ".raw"):
		// Handle cases like "talos.raw.xz" when only checking extension
		return NewRAWSource(path), nil
	default:
		return nil, errors.Newf("unknown image format: %s (expected .iso, .raw, .raw.xz, .raw.zst, or container reference)", path)
	}
}
