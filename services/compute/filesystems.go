package compute

import (
	"context"
	"fmt"

	"google.golang.org/grpc"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/compute/api"
	"github.com/kfirgollan/NebiusMocker/services/compute/backend/fsstore"
	"github.com/kfirgollan/NebiusMocker/services/compute/dal"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

// newFilesystemAttachments builds the port InstanceService attaches shared
// filesystems through (spec.filesystems[].existing_filesystem).
func newFilesystemAttachments(deps mocker.Deps) (*domain.FilesystemAttach, error) {
	att, err := domain.NewFilesystemAttachments(dal.NewFilesystemRepo(deps.Store))
	if err != nil {
		return nil, fmt.Errorf("compute: %w", err)
	}
	return att, nil
}

// registerFilesystems wires FilesystemService and provides the peer
// capability Object Storage filesystem buckets read files through.
func registerFilesystems(reg grpc.ServiceRegistrar, deps mocker.Deps) error {
	repo := dal.NewFilesystemRepo(deps.Store)
	data := fsstore.New(deps.ObjStore)
	svc, err := domain.NewFilesystemService(domain.FilesystemDeps{
		Filesystems: repo, OpIndex: dal.NewFilesystemOpIndex(deps.Store), Data: data,
		Buckets:  filesystemBucketUsers{peers: deps.Peers},
		Projects: deps.Authz, Ops: deps.Ops, IDs: deps.IDs, Clock: deps.Clock,
	})
	if err != nil {
		return fmt.Errorf("compute: %w", err)
	}
	deps.Authz.RegisterKind(domain.KindFilesystem, func(ctx context.Context, id string) (string, error) {
		f, err := repo.Get(ctx, id)
		return f.GetMetadata().GetParentId(), err
	})
	api.RegisterFilesystemAuthzRules(deps.Rules)
	computepb.RegisterFilesystemServiceServer(reg, api.NewFilesystemHandler(svc))
	if err := peer.Provide[peer.Filesystems](deps.Peers, peer.FilesystemsKey, filesystemPeer{repo: repo, data: data}); err != nil {
		return fmt.Errorf("compute: %w", err)
	}
	return nil
}

// filesystemBucketUsers asks the storage module, when enabled, which
// filesystem buckets expose a filesystem. The lookup happens at call time
// because module order is not guaranteed.
type filesystemBucketUsers struct{ peers *peer.Directory }

func (b filesystemBucketUsers) BucketsOf(ctx context.Context, fsID string) ([]string, error) {
	users, ok := peer.Lookup(b.peers, peer.FilesystemBucketsKey)
	if !ok {
		return nil, nil
	}
	return users.BucketsOf(ctx, fsID)
}

// filesystemPeer is the peer.Filesystems capability of this module.
type filesystemPeer struct {
	repo domain.FilesystemRepository
	data domain.FilesystemData
}

func (p filesystemPeer) Filesystem(ctx context.Context, id string) (peer.Filesystem, error) {
	f, err := p.repo.Get(ctx, id)
	if err != nil {
		return peer.Filesystem{}, err
	}
	st := f.GetStatus().GetState()
	return peer.Filesystem{
		ID: id, ParentID: f.GetMetadata().GetParentId(),
		Ready:         st == computepb.FilesystemStatus_READY || st == computepb.FilesystemStatus_UPDATING,
		CapacityBytes: f.GetStatus().GetSizeBytes(),
		BackendBucket: p.data.Bucket(id),
	}, nil
}
