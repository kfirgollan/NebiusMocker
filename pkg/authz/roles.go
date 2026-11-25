// Package authz decides whether a principal may perform an action on a resource.
//
// The model (docs/architecture/06-iam-and-authorization.md):
//   - every RPC needs one Permission, "<service>.<resources>.<verb>";
//   - a Role grants permission patterns;
//   - Bindings say which roles a principal holds on a resource (in Nebius:
//     group memberships plus access permits);
//   - a role held on a resource also applies to every descendant, so the
//     engine walks the resource's ancestry (e.g. group → tenant).
//
// The package is transport-free. The gRPC interceptor that enforces decisions
// lives in pkg/interceptors.
package authz

import (
	"slices"
	"strings"
)

// Permission is "<service>.<resources>.<verb>", e.g. "iam.groups.create".
type Permission string

// Verbs used by the standard methods.
const (
	VerbGet    = "get"
	VerbList   = "list"
	VerbCreate = "create"
	VerbUpdate = "update"
	VerbDelete = "delete"
)

// Lifecycle verbs of runnable resources (AI endpoints, and later compute
// instances). Editors hold them: running a workload is managing it.
const (
	VerbStart   = "start"
	VerbStop    = "stop"
	VerbRestart = "restart"
)

// Perm builds a permission from its parts.
func Perm(service, resources, verb string) Permission {
	return Permission(service + "." + resources + "." + verb)
}

// Verb returns the last segment of the permission.
func (p Permission) Verb() string {
	s := string(p)
	return s[strings.LastIndexByte(s, '.')+1:]
}

// Role grants a set of permission patterns. A pattern is either "*" (every
// permission) or three dot-separated segments where "*" matches any one
// segment, e.g. "*.*.get". Excludes are checked first and win.
type Role struct {
	Name     string
	Grants   []string
	Excludes []string
}

// Allows reports whether the role grants p.
func (r Role) Allows(p Permission) bool {
	if slices.ContainsFunc(r.Excludes, func(pat string) bool { return match(pat, p) }) {
		return false
	}
	return slices.ContainsFunc(r.Grants, func(pat string) bool { return match(pat, p) })
}

func match(pattern string, p Permission) bool {
	if pattern == "*" {
		return true
	}
	pats := strings.Split(pattern, ".")
	segs := strings.Split(string(p), ".")
	if len(pats) != len(segs) {
		return false
	}
	for i := range pats {
		if pats[i] != "*" && pats[i] != segs[i] {
			return false
		}
	}
	return true
}

// Built-in role names. These are the mocker's model of Nebius roles; the public
// API carries roles only as strings (AccessPermitSpec.role).
const (
	RoleViewer = "viewer"
	RoleEditor = "editor"
	RoleAdmin  = "admin"
	// RoleMysteryBoxPayloadViewer may read MysteryBox secret payloads, which
	// viewer and editor may not.
	RoleMysteryBoxPayloadViewer = "mysterybox.payload-viewer"
)

// permMysteryBoxPayloadRead reads decrypted secret payloads. Its verb is not
// "get", so the viewer and editor read patterns do not grant it (Nebius docs:
// only admin and mysterybox.payload-viewer may read payloads).
const permMysteryBoxPayloadRead = "mysterybox.payload.read"

// Catalog is the set of known roles.
type Catalog struct {
	roles map[string]Role
}

// NewCatalog returns a catalog of the given roles.
func NewCatalog(roles ...Role) *Catalog {
	c := &Catalog{roles: map[string]Role{}}
	for _, r := range roles {
		c.roles[r.Name] = r
	}
	return c
}

// DefaultCatalog returns viewer, editor, admin and the service roles.
func DefaultCatalog() *Catalog {
	read := []string{"*.*." + VerbGet, "*.*." + VerbList}
	return NewCatalog(
		Role{Name: RoleViewer, Grants: read},
		Role{
			Name: RoleEditor,
			Grants: append(slices.Clone(read), "*.*."+VerbCreate, "*.*."+VerbUpdate, "*.*."+VerbDelete,
				// Editors use KMS keys and manage their lifecycle (Nebius KMS
				// docs: crypto operations need the editor role). Viewers can
				// read keys and public keys but cannot encrypt, decrypt or sign.
				"kms.*.encrypt", "kms.*.decrypt", "kms.*.sign", "kms.*.rotate", "kms.*.undelete",
				"*.*."+VerbStart, "*.*."+VerbStop, "*.*."+VerbRestart),
			// Editors manage resources, not who may access them.
			Excludes: []string{"iam.accessPermits.create", "iam.accessPermits.delete", "iam.groupMemberships.create", "iam.groupMemberships.delete"},
		},
		Role{Name: RoleAdmin, Grants: []string{"*"}},
		Role{Name: RoleMysteryBoxPayloadViewer, Grants: []string{permMysteryBoxPayloadRead}},
	)
}

// Has reports whether the role exists.
func (c *Catalog) Has(name string) bool {
	_, ok := c.roles[name]
	return ok
}

// Allows reports whether the named role grants p. Unknown roles grant nothing.
func (c *Catalog) Allows(role string, p Permission) bool {
	r, ok := c.roles[role]
	return ok && r.Allows(p)
}
