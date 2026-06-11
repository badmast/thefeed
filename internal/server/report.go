package server

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// reportFile appends one JSON line per hourly report to a file and rotates it
// by size, keeping a bounded number of backups. It is the canonical source
// for the `--report` dashboard (which reads the file instead of scraping logs).
// Pure-Go, no external dependency.
type reportFile struct {
	mu         sync.Mutex
	path       string
	maxBytes   int64
	maxBackups int
	f          *os.File
	size       int64
}

const (
	reportMaxBytes   = 4 << 20 // 4 MiB before rotation (~thousands of hourly lines)
	reportMaxBackups = 5       // dns_hourly.jsonl.1 .. .5
)

// openReportFile opens (creating its directory) the rotating report file at
// path. A nil return with a nil error means reporting-to-file is disabled
// (empty path).
func openReportFile(path string) (*reportFile, error) {
	if path == "" {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("report dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open report file: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &reportFile{
		path:       path,
		maxBytes:   reportMaxBytes,
		maxBackups: reportMaxBackups,
		f:          f,
		size:       info.Size(),
	}, nil
}

// Append writes one record as a single line (a trailing newline is added).
func (r *reportFile) Append(line []byte) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.size+int64(len(line))+1 > r.maxBytes {
		if err := r.rotateLocked(); err != nil {
			return err
		}
	}
	n, err := r.f.Write(append(line, '\n'))
	r.size += int64(n)
	return err
}

// rotateLocked closes the current file, shifts backups (.N-1 -> .N, dropping
// the oldest), renames the current file to .1, and opens a fresh file.
func (r *reportFile) rotateLocked() error {
	if err := r.f.Close(); err != nil {
		return err
	}
	// Drop the oldest, then shift each backup up by one.
	oldest := fmt.Sprintf("%s.%d", r.path, r.maxBackups)
	_ = os.Remove(oldest)
	for i := r.maxBackups - 1; i >= 1; i-- {
		from := fmt.Sprintf("%s.%d", r.path, i)
		to := fmt.Sprintf("%s.%d", r.path, i+1)
		if _, err := os.Stat(from); err == nil {
			_ = os.Rename(from, to)
		}
	}
	if err := os.Rename(r.path, r.path+".1"); err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	r.f = f
	r.size = 0
	return nil
}

// Close closes the underlying file.
func (r *reportFile) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}
