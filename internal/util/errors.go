package util

import (
	"errors"
	"io"
	"log"
	"net"
	"os"
	"runtime"
	"strings"
)

// Logerr logs an error if it's non-nil, including the source file and line number.
// Intended for use with defer to handle errors from operations like Close().
//
// The optional helperFrames parameter specifies additional stack frames to skip
// when reporting the error location (useful for helper functions that wrap Logerr).
//
// Expected network errors (like "use of closed network connection") are silently ignored.
//
// Example:
//
//	defer func() { Logerr(f.Close()) }()
func Logerr(err error, helperFrames ...int) {
	if err == nil {
		return
	}

	// Ignore expected network errors that are normal in proxy/server operations
	if isExpectedNetworkError(err) {
		return
	}

	frames := 1 // default: caller of Logerr
	if len(helperFrames) > 0 {
		frames += helperFrames[0]
	}
	_, file, line, ok := runtime.Caller(frames)
	if !ok {
		file = "???"
		line = 0
	}
	log.Printf("%s:%d: deferred operation failed: %v", file, line, err)
}

// isExpectedNetworkError returns true for errors that are expected in network operations
// and don't need to be logged (e.g., connection already closed by remote side)
func isExpectedNetworkError(err error) bool {
	if err == nil {
		return true
	}

	// Check for common expected network errors
	errStr := err.Error()
	expectedPatterns := []string{
		"use of closed network connection",
		"broken pipe",
		"connection reset by peer",
		"connection refused", // Often expected when services are down
	}

	for _, pattern := range expectedPatterns {
		if strings.Contains(errStr, pattern) {
			return true
		}
	}

	// Check for specific error types
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		// Network operation errors during close are often expected
		if netErr.Op == "close" {
			return true
		}
	}

	return false
}

// DeferClose closes an io.Closer and logs any error with the caller's location.
// Convenience wrapper around Logerr for the common case of deferred Close().
//
// Example:
//
//	defer DeferClose(f)
func DeferClose(c io.Closer) {
	if c != nil {
		Logerr(c.Close(), 1) // Skip 1 extra frame to show DeferClose's caller
	}
}

// DeferRemove removes a file and logs any error with the caller's location.
// Convenience wrapper around Logerr for the common case of deferred file cleanup.
// Ignores "no such file" errors since the file may have been successfully moved.
//
// Example:
//
//	defer util.DeferRemove(tempPath)
func DeferRemove(path string) {
	if path != "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			Logerr(err, 1) // Skip 1 extra frame to show DeferRemove's caller
		}
	}
}

// LogerrT is a generic helper for operations that return (T, error).
// It logs the error if non-nil and returns the value T.
// Useful for operations like io.Copy, Write, etc. where you want to check
// the error but may not need the return value.
//
// Example:
//
//	bytesWritten := util.LogerrT(io.Copy(dest, src))  // Use the return value
//	util.LogerrT(w.Write(data))                       // Ignore the return value
//	util.LogerrT(io.Copy(io.Discard, tarReader))      // Discard both value and error (after checking)
func LogerrT[T any](val T, err error) T {
	Logerr(err, 1) // Skip 1 extra frame to show LogerrT's caller
	return val
}
