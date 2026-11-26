package main

import (
	"fmt"
	"os"
	"sync"
)

// rotatingFile is a size-bounded append-only log writer: when the next write would exceed maxBytes the
// file is rotated (path -> path.1 -> path.2 ... up to `keep` backups, the oldest is deleted). It keeps
// decision logs bounded on long-running deployments.
type rotatingFile struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	keep     int
	f        *os.File
	size     int64
}

func newRotatingFile(path string, maxBytes int64, keep int) (*rotatingFile, error) {
	if maxBytes < 1024 || keep < 1 {
		return nil, fmt.Errorf("rotatingFile: maxBytes must be >= 1024 and keep >= 1")
	}
	r := &rotatingFile{path: path, maxBytes: maxBytes, keep: keep}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *rotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) // owner-only: logs may hold decision data
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, st.Size()
	return nil
}

func (r *rotatingFile) rotate() error {
	if r.f != nil {
		_ = r.f.Close()
		r.f = nil
	}
	_ = os.Remove(fmt.Sprintf("%s.%d", r.path, r.keep))
	for i := r.keep - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", r.path, i), fmt.Sprintf("%s.%d", r.path, i+1))
	}
	_ = os.Rename(r.path, r.path+".1")
	return r.open()
}

// Write implements io.Writer.
func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return 0, os.ErrClosed
	}
	if r.size > 0 && r.size+int64(len(p)) > r.maxBytes {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// Close flushes and closes the file.
func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}
