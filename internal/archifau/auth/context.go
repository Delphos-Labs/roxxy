package auth

import "context"

type identityCtxKey struct{}

// WithIdentity returns ctx carrying the authenticated identity, so handlers
// downstream of the authentication middleware (including the reused docker
// registry) can make authorization decisions without re-verifying.
func WithIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, identityCtxKey{}, id)
}

// IdentityFromContext returns the identity placed by WithIdentity, or nil when
// none is present (e.g. an unauthenticated request, or a caller that does not
// set it — in which case authorization callbacks should fail closed).
func IdentityFromContext(ctx context.Context) *Identity {
	id, _ := ctx.Value(identityCtxKey{}).(*Identity)
	return id
}
