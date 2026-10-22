package server

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/archifau/audit"
)

// accessLog is the outermost middleware: it threads a per-request audit
// record through the context, runs the handler with a status/bytes-capturing
// ResponseWriter, then emits exactly one structured access line.
func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		ctx, rec := audit.NewContext(r.Context())
		r = r.WithContext(ctx)

		rw, counter := wrapResponseWriter(w)
		next.ServeHTTP(rw, r)

		status := counter.status
		if status == 0 {
			status = http.StatusOK // handler wrote nothing at all
		}

		level := slog.LevelInfo
		switch {
		case r.URL.Path == "/healthz":
			level = slog.LevelDebug // compose healthcheck polls it
		case status >= 500:
			level = slog.LevelError
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			level = slog.LevelWarn
		}

		attrs := []slog.Attr{
			slog.String("remote_addr", remoteAddr(r)),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("proto", r.Proto),
			slog.Int("status", status),
			slog.Int64("bytes", counter.bytes),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			slog.String("user_agent", r.UserAgent()),
		}
		attrs = append(attrs, rec.Attrs()...)

		slog.LogAttrs(r.Context(), level, "access", attrs...)
	})
}

func remoteAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// baseRW captures status and bytes written. ReadFrom deliberately lives on a
// separate wrapper type: declaring it here would claim io.ReaderFrom even
// when the underlying writer lacks it, breaking io.Copy's fast-path probe.
type baseRW struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (rw *baseRW) WriteHeader(code int) {
	if rw.status == 0 {
		rw.status = code
	}
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *baseRW) Write(b []byte) (int, error) {
	if rw.status == 0 {
		rw.status = http.StatusOK
	}
	n, err := rw.ResponseWriter.Write(b)
	rw.bytes += int64(n)
	return n, err
}

func (rw *baseRW) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// readerFromRW preserves the io.Copy fast path (sendfile) that http.ServeFile
// relies on, while still counting bytes. Only constructed when the underlying
// writer implements io.ReaderFrom.
type readerFromRW struct{ *baseRW }

func (rw readerFromRW) ReadFrom(src io.Reader) (int64, error) {
	if rw.status == 0 {
		rw.status = http.StatusOK
	}
	n, err := rw.ResponseWriter.(io.ReaderFrom).ReadFrom(src)
	rw.bytes += n
	return n, err
}

// wrapResponseWriter returns a writer that implements io.ReaderFrom only when
// the underlying one does, so http.ServeFile behaves identically to the
// unwrapped case, plus the counter to read status/bytes from afterwards.
func wrapResponseWriter(w http.ResponseWriter) (http.ResponseWriter, *baseRW) {
	b := &baseRW{ResponseWriter: w}
	if _, ok := w.(io.ReaderFrom); ok {
		return readerFromRW{b}, b
	}
	return b, b
}
