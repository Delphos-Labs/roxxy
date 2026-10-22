package util

import (
	"fmt"
	"io"
	"time"
)

// ActivityReader wraps an io.Reader with inactivity timeout
// If no data arrives within timeout, read operations fail
// This allows unlimited total download time as long as chunks keep flowing
type ActivityReader struct {
	reader  io.Reader
	timeout time.Duration
	timer   *time.Timer
	done    chan struct{}
}

// NewActivityReader creates a new reader with activity-based timeout
// If timeout is 0, defaults to 2 minutes
func NewActivityReader(reader io.Reader, timeout time.Duration) *ActivityReader {
	if timeout == 0 {
		timeout = 2 * time.Minute
	}

	return &ActivityReader{
		reader:  reader,
		timeout: timeout,
		timer:   time.NewTimer(timeout),
		done:    make(chan struct{}),
	}
}

func (ar *ActivityReader) Read(p []byte) (n int, err error) {
	type readResult struct {
		n   int
		err error
	}
	resultCh := make(chan readResult, 1)

	go func() {
		n, err := ar.reader.Read(p)
		select {
		case resultCh <- readResult{n, err}:
		case <-ar.done:
		}
	}()

	select {
	case result := <-resultCh:
		if result.n > 0 {
			// Reset timer on successful read
			if !ar.timer.Stop() {
				select {
				case <-ar.timer.C:
				default:
				}
			}
			ar.timer.Reset(ar.timeout)
		}
		return result.n, result.err
	case <-ar.timer.C:
		close(ar.done)
		return 0, fmt.Errorf("read timeout: no data received for %v", ar.timeout)
	}
}

func (ar *ActivityReader) Close() error {
	if !ar.timer.Stop() {
		select {
		case <-ar.timer.C:
		default:
		}
	}
	select {
	case <-ar.done:
	default:
		close(ar.done)
	}
	return nil
}
