package api

import (
	"context"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
)

const imageService = "nebius.compute.v1.ImageService"

// PublicImages answers whether a resource belongs to the public catalog
// (satisfied by *domain.ImageService).
type PublicImages interface {
	IsPublicProject(id string) bool
	IsPublicImage(ctx context.Context, id string) bool
}

type parentGetter interface{ GetParentId() string }

// RegisterAuthzRules declares the permission each ImageService RPC needs.
// Custom images are governed by the roles on their project (compute.images.*).
// Public images are readable by any authenticated principal: reads that
// target a public-images project or a public image are allowed outright.
func RegisterAuthzRules(r *authz.Rules, pub PublicImages) {
	r.CRUD(imageService, "compute", "images")
	m := func(method string) string { return "/" + imageService + "/" + method }
	read := authz.Perm("compute", "images", authz.VerbGet)
	list := authz.Perm("compute", "images", authz.VerbList)

	publicParent := func(_ context.Context, _ string, req any) bool {
		p, ok := req.(parentGetter)
		return ok && pub.IsPublicProject(p.GetParentId())
	}
	withAllow := func(rule authz.Rule, allow func(context.Context, string, any) bool) authz.Rule {
		rule.AllowIf = allow
		return rule
	}

	r.Add(m("Get"), withAllow(authz.OnID(read), func(ctx context.Context, _ string, req any) bool {
		g, ok := req.(*computepb.GetImageRequest)
		return ok && pub.IsPublicImage(ctx, g.GetId())
	}))
	r.Add(m("GetByName"), withAllow(authz.OnParent(read), publicParent))
	r.Add(m("List"), withAllow(authz.OnParent(list), publicParent))
	// An empty parent_id means the default public-images project.
	r.Add(m("GetLatestByFamily"), withAllow(authz.OnParent(read), func(ctx context.Context, principal string, req any) bool {
		g, ok := req.(*computepb.GetImageLatestByFamilyRequest)
		return ok && (g.GetParentId() == "" || publicParent(ctx, principal, req))
	}))
	r.Add(m("ListPublic"), authz.Rule{
		Permission: list,
		AllowIf:    func(context.Context, string, any) bool { return true },
		Target:     func(context.Context, any) (string, error) { return "", nil },
	})
	r.Add(m("ListOperationsByParent"), authz.OnParent(list))

	// Writes aimed at the public catalog pass authorization (the public-images
	// projects are not IAM resources) and are refused by the domain with
	// PERMISSION_DENIED: public images are read-only for everyone.
	r.Add(m("Create"), withAllow(authz.OnMetadataParent(authz.Perm("compute", "images", authz.VerbCreate)),
		func(_ context.Context, _ string, req any) bool {
			c, ok := req.(*computepb.CreateImageRequest)
			return ok && pub.IsPublicProject(c.GetMetadata().GetParentId())
		}))
	r.Add(m("Update"), withAllow(authz.OnMetadataID(authz.Perm("compute", "images", authz.VerbUpdate)),
		func(ctx context.Context, _ string, req any) bool {
			u, ok := req.(*computepb.UpdateImageRequest)
			return ok && pub.IsPublicImage(ctx, u.GetMetadata().GetId())
		}))
	r.Add(m("Delete"), withAllow(authz.OnID(authz.Perm("compute", "images", authz.VerbDelete)),
		func(ctx context.Context, _ string, req any) bool {
			d, ok := req.(*computepb.DeleteImageRequest)
			return ok && pub.IsPublicImage(ctx, d.GetId())
		}))
}
