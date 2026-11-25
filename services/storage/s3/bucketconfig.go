package s3

import (
	"context"
	"net/http"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

// configReq is one bucket call answered from Nebius metadata.
type configReq struct {
	w  http.ResponseWriter
	b  *storagepb.Bucket
	bd body
}

func (c configReq) id() string                  { return c.b.GetMetadata().GetId() }
func (c configReq) spec() *storagepb.BucketSpec { return c.b.GetSpec() }

type configHandler func(ctx context.Context, s *Server, c configReq) error

// configHandlers serve bucket management and configuration over S3 from and
// into the Nebius bucket record (the object store never sees them).
var configHandlers = map[domain.S3Action]configHandler{
	domain.ActGetBucketLocation: func(_ context.Context, s *Server, c configReq) error {
		writeXML(c.w, http.StatusOK, locationConstraint{Xmlns: s3NS, Region: s.cfg.Region})
		return nil
	},
	domain.ActDeleteBucket: func(ctx context.Context, s *Server, c configReq) error {
		if err := s.buckets.DeleteFromS3(ctx, c.id()); err != nil {
			if errs.IsKind(err, errs.KindBadState) {
				return coded("BucketNotEmpty", http.StatusConflict, "The bucket you tried to delete is not empty")
			}
			return err
		}
		c.w.WriteHeader(http.StatusNoContent)
		return nil
	},
	domain.ActGetBucketVersioning: func(_ context.Context, _ *Server, c configReq) error {
		return writeConfig(c.w)(versioningXML(c.spec().GetVersioningPolicy()))
	},
	domain.ActPutBucketVersioning: putConfig(parseVersioningXML, func(sp *storagepb.BucketSpec, p storagepb.VersioningPolicy) {
		sp.VersioningPolicy = p
	}),
	domain.ActGetBucketLifecycle: func(_ context.Context, _ *Server, c configReq) error {
		if len(c.spec().GetLifecycleConfiguration().GetRules()) == 0 {
			return coded("NoSuchLifecycleConfiguration", http.StatusNotFound, "The lifecycle configuration does not exist")
		}
		return writeConfig(c.w)(lifecycleXML(c.spec().GetLifecycleConfiguration()))
	},
	domain.ActPutBucketLifecycle: putConfig(parseLifecycleXML, func(sp *storagepb.BucketSpec, cfg *storagepb.LifecycleConfiguration) {
		sp.LifecycleConfiguration = cfg
	}),
	domain.ActDeleteBucketLifecycle: deleteConfig(func(sp *storagepb.BucketSpec) { sp.LifecycleConfiguration = nil }),
	domain.ActGetBucketCors: func(_ context.Context, _ *Server, c configReq) error {
		if len(c.spec().GetCors().GetRules()) == 0 {
			return coded("NoSuchCORSConfiguration", http.StatusNotFound, "The CORS configuration does not exist")
		}
		return writeConfig(c.w)(corsXML(c.spec().GetCors()))
	},
	domain.ActPutBucketCors: putConfig(parseCORSXML, func(sp *storagepb.BucketSpec, cfg *storagepb.CORSConfiguration) {
		sp.Cors = cfg
	}),
	domain.ActDeleteBucketCors: deleteConfig(func(sp *storagepb.BucketSpec) { sp.Cors = nil }),
}

// putConfig parses an XML document and stores it into the spec.
func putConfig[T any](parse func([]byte) (T, error), set func(*storagepb.BucketSpec, T)) configHandler {
	return func(ctx context.Context, s *Server, c configReq) error {
		raw, err := readBody(c.bd)
		if err != nil {
			return err
		}
		v, err := parse(raw)
		if err != nil {
			return err
		}
		if err := s.buckets.UpdateSpecFromS3(ctx, c.id(), func(sp *storagepb.BucketSpec) { set(sp, v) }); err != nil {
			return err
		}
		c.w.WriteHeader(http.StatusOK)
		return nil
	}
}

func deleteConfig(reset func(*storagepb.BucketSpec)) configHandler {
	return func(ctx context.Context, s *Server, c configReq) error {
		if err := s.buckets.UpdateSpecFromS3(ctx, c.id(), reset); err != nil {
			return err
		}
		c.w.WriteHeader(http.StatusNoContent)
		return nil
	}
}

func writeConfig(w http.ResponseWriter) func([]byte, error) error {
	return func(out []byte, err error) error {
		if err != nil {
			return err
		}
		writeRaw(w, http.StatusOK, out)
		return nil
	}
}
