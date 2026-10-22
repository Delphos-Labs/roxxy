// Package auth implements archifau's authentication and authorization.
//
// GitHub is the only identity provider. Two credential kinds are accepted:
//
//   - a GitHub Actions OIDC ID token (a signed JWT, verified offline against
//     GitHub's JWKS) — identity is the repository that ran the job;
//   - a GitHub user token (gh auth token / PAT, verified by calling the
//     GitHub API) — identity is the user plus the allowed orgs they belong to.
//
// Both resolve to an Identity, and all authorization derives from it: CI jobs
// are confined to their repository's namespace (writes only from main),
// humans to the namespaces of allowed orgs they are members of. After the
// initial exchange archifau mints its own short-lived token (see mint.go) so
// per-request verification needs no network access.
package auth

import "strings"

// Identity is what authentication established about the caller.
type Identity struct {
	// Kind is "oidc" (GitHub Actions job) or "user" (gh/PAT token holder).
	Kind string `json:"kind"`
	// Subject is "org/repo" for oidc identities, "username" for users.
	// Always lowercase.
	Subject string `json:"sub"`
	// Repository is the lowercased org/repo claim (oidc only).
	Repository string `json:"repository,omitempty"`
	// Ref is the git ref the OIDC token was minted for (oidc only).
	Ref string `json:"ref,omitempty"`
	// Actor is the GitHub username that triggered the workflow, or the
	// authenticated user for user identities.
	Actor string `json:"actor,omitempty"`
	// Workflow is the workflow name (oidc only).
	Workflow string `json:"workflow,omitempty"`
	// Namespaces the identity may operate in, lowercase. For oidc this is
	// the single repository ("org/repo"); for users, the allowed orgs they
	// belong to (plus their own username when it is an allowed owner).
	Namespaces []string `json:"namespaces"`
}

const mainRef = "refs/heads/main"

func inNamespace(name, ns string) bool {
	return name == ns || strings.HasPrefix(name, ns+"/")
}

// CanRead reports whether the identity may pull/fetch the named resource
// (a docker repository or file path, lowercase, no leading slash).
func (id *Identity) CanRead(name string) bool {
	name = strings.ToLower(strings.Trim(name, "/"))
	for _, ns := range id.Namespaces {
		if inNamespace(name, ns) {
			return true
		}
	}
	return false
}

// CanWrite reports whether the identity may push/upload the named resource.
// Mirrors the original docker_auth ACL: CI may write only from refs/heads/main
// and only inside its own repository namespace; humans anywhere in their orgs.
func (id *Identity) CanWrite(name string) bool {
	if id.Kind == "oidc" && id.Ref != mainRef {
		return false
	}
	return id.CanRead(name)
}

// CanWriteFlat is the write check for flat namespaces (PyPI uploads), where
// there is no per-repo prefix to confine writes to.
func (id *Identity) CanWriteFlat() bool {
	if id.Kind == "oidc" && id.Ref != mainRef {
		return false
	}
	return len(id.Namespaces) > 0
}
