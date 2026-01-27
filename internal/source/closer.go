package source

import (
	"io"
	"os"
	"sync"
)

// sharedCloser wraps a closer with sync.Once to prevent double-close.
// It also optionally cleans up directories on close.
type sharedCloser struct {
	closer      io.Closer
	cleanupDirs []string
	once        sync.Once
	err         error
}

// newSharedCloser creates a new sharedCloser.
// closer can be nil if only directory cleanup is needed.
func newSharedCloser(closer io.Closer, cleanupDir string) *sharedCloser {
	var dirs []string
	if cleanupDir != "" {
		dirs = []string{cleanupDir}
	}
	return &sharedCloser{closer: closer, cleanupDirs: dirs}
}

// newSharedCloserMulti creates a sharedCloser with multiple cleanup directories.
func newSharedCloserMulti(closer io.Closer, cleanupDirs []string) *sharedCloser {
	return &sharedCloser{closer: closer, cleanupDirs: cleanupDirs}
}

// Close closes the underlying closer and removes all cleanup directories.
// Safe to call multiple times - only the first call has effect.
func (s *sharedCloser) Close() error {
	s.once.Do(func() {
		if s.closer != nil {
			s.err = s.closer.Close()
		}
		for _, dir := range s.cleanupDirs {
			os.RemoveAll(dir)
		}
	})
	return s.err
}

// readerCloser wraps a reader with a shared closer.
type readerCloser struct {
	reader io.Reader
	closer interface{ Close() error }
}

func (r *readerCloser) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

func (r *readerCloser) Close() error {
	return r.closer.Close()
}

// filesCloser handles cleanup for multiple open files.
type filesCloser struct {
	files      []*os.File
	cleanupDir string
	once       sync.Once
	err        error
}

func newFilesCloser(files []*os.File, cleanupDir string) *filesCloser {
	return &filesCloser{files: files, cleanupDir: cleanupDir}
}

func (s *filesCloser) Close() error {
	s.once.Do(func() {
		for _, f := range s.files {
			f.Close()
		}
		if s.cleanupDir != "" {
			os.RemoveAll(s.cleanupDir)
		}
	})
	return s.err
}
