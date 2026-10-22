// Package audit carries a per-request audit record through the request
// context. The access-log middleware creates one record per request and emits
// it as a single structured line at completion; auth and storage handlers
// enrich it along the way (identity, event, object, content hash, deny
// reason). All helpers are nil-safe so handlers behave when invoked without
// the middleware (e.g. direct unit tests).
package audit

import (
	"context"
	"log/slog"
)

// Record is the mutable per-request audit accumulator. All mutation happens
// on the single goroutine serving the request, so no locking is needed.
type Record struct {
	Event   string // authenticated, denied, anonymous, upload, download, delete, token_issued
	Outcome string // allowed, denied, unauthenticated
	Reason  string // deny/reject reason
	Object  string // resource acted on: repo, file rel path, "pkg/file.whl"
	SHA256  string // content hash for upload/download success
	ObjSize int64  // object size in bytes for upload/download

	// Identity, copied out of auth.Identity as plain strings so this package
	// never imports auth.
	IDKind    string // "oidc" or "user"
	IDSubject string
	IDActor   string
	IDRef     string
}

type ctxKey struct{}

// NewContext returns ctx carrying a fresh Record and the record itself.
func NewContext(ctx context.Context) (context.Context, *Record) {
	rec := &Record{}
	return context.WithValue(ctx, ctxKey{}, rec), rec
}

// FromContext returns the request's Record, or nil when no middleware ran.
func FromContext(ctx context.Context) *Record {
	rec, _ := ctx.Value(ctxKey{}).(*Record)
	return rec
}

// SetIdentity records the authenticated identity fields.
func SetIdentity(ctx context.Context, kind, subject, actor, ref string) {
	if rec := FromContext(ctx); rec != nil {
		rec.IDKind, rec.IDSubject, rec.IDActor, rec.IDRef = kind, subject, actor, ref
		if rec.Event == "" {
			rec.Event, rec.Outcome = "authenticated", "allowed"
		}
	}
}

// Anonymous marks a request that presented no or invalid credentials.
func Anonymous(ctx context.Context) {
	if rec := FromContext(ctx); rec != nil {
		rec.Event, rec.Outcome = "anonymous", "unauthenticated"
	}
}

// Denied marks an authorization failure with a reason.
func Denied(ctx context.Context, reason string) {
	if rec := FromContext(ctx); rec != nil {
		rec.Event, rec.Outcome, rec.Reason = "denied", "denied", reason
	}
}

// Event sets the action verb and the object it acted on.
func Event(ctx context.Context, event, object string) {
	if rec := FromContext(ctx); rec != nil {
		rec.Event, rec.Object = event, object
	}
}

// Object sets the acted-on resource without changing the event.
func Object(ctx context.Context, object string) {
	if rec := FromContext(ctx); rec != nil {
		rec.Object = object
	}
}

// Content attaches a content hash and size to an upload/download record.
func Content(ctx context.Context, sha256 string, size int64) {
	if rec := FromContext(ctx); rec != nil {
		rec.SHA256, rec.ObjSize = sha256, size
	}
}

// Attrs returns the audit fields as slog attributes, omitting empty ones.
func (rec *Record) Attrs() []slog.Attr {
	if rec == nil {
		return nil
	}
	a := make([]slog.Attr, 0, 10)
	if rec.Event != "" {
		a = append(a, slog.String("event", rec.Event))
	}
	if rec.Outcome != "" {
		a = append(a, slog.String("outcome", rec.Outcome))
	}
	if rec.Reason != "" {
		a = append(a, slog.String("reason", rec.Reason))
	}
	if rec.Object != "" {
		a = append(a, slog.String("object", rec.Object))
	}
	if rec.SHA256 != "" {
		a = append(a, slog.String("sha256", rec.SHA256))
	}
	if rec.ObjSize > 0 {
		a = append(a, slog.Int64("object_size", rec.ObjSize))
	}
	if rec.IDKind != "" || rec.IDSubject != "" {
		a = append(a,
			slog.String("id_kind", rec.IDKind),
			slog.String("id_subject", rec.IDSubject),
			slog.String("id_actor", rec.IDActor),
			slog.String("id_ref", rec.IDRef),
		)
	}
	return a
}
