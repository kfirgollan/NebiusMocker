package domain

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	pgpb "github.com/nebius/gosdk/proto/nebius/msp/postgresql/v1alpha1"
	mspv1alpha1pb "github.com/nebius/gosdk/proto/nebius/msp/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

const networkField = "spec.network_id"

// CreateClusterCmd is the input of CreateCluster.
type CreateClusterCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *pgpb.ClusterSpec
	Request  proto.Message
}

// UpdateClusterCmd is the input of UpdateCluster (a patch guarded by a reset
// mask).
type UpdateClusterCmd struct {
	Metadata  *commonpb.ResourceMetadata
	Spec      *pgpb.ClusterSpec
	ResetMask *mask.Mask
	Request   proto.Message
}

// RestoreClusterCmd is the input of RestoreCluster.
type RestoreClusterCmd struct {
	Metadata        *commonpb.ResourceMetadata
	Spec            *pgpb.ClusterSpec
	SourceClusterID string
	BackupID        string
	// RecoveryTime is the point-in-time target; nil restores the backup as
	// taken.
	RecoveryTime *time.Time
	Request      proto.Message
}

// GetCluster returns a cluster.
func (s *Service) GetCluster(ctx context.Context, id string) (*pgpb.Cluster, error) {
	return s.d.Clusters.Get(ctx, id)
}

// GetClusterByName returns the cluster with the name in a project.
func (s *Service) GetClusterByName(ctx context.Context, parentID, name string) (*pgpb.Cluster, error) {
	return s.d.Clusters.GetByName(ctx, parentID, name)
}

// ListClusters returns one page of a project's clusters in creation order.
func (s *Service) ListClusters(ctx context.Context, parentID string, size int64, pageToken string) (Page[*pgpb.Cluster], error) {
	n, err := pageSize(size)
	if err != nil {
		return Page[*pgpb.Cluster]{}, err
	}
	return s.d.Clusters.List(ctx, parentID, n, pageToken)
}

// stripPassword returns spec without bootstrap.user_password, and the
// password: it is never stored.
func stripPassword(spec *pgpb.ClusterSpec) (stripped *pgpb.ClusterSpec, password string) {
	stripped = proto.CloneOf(spec)
	password = stripped.GetBootstrap().GetUserPassword()
	if stripped.GetBootstrap() != nil {
		stripped.Bootstrap.UserPassword = ""
	}
	return stripped, password
}

// run is what one ensureCluster call needs beyond the stored cluster.
type run struct {
	// password is the bootstrap password of the request ("" keeps it).
	password string
	restore  *RestoreSource
	// firstBackup takes the automatic base backup once the cluster runs.
	firstBackup bool
}

// CreateCluster creates a cluster: PROVISIONING until the backend's hosts are
// ready, then RUNNING with its endpoints.
func (s *Service) CreateCluster(ctx context.Context, cmd CreateClusterCmd) (*operations.Operation, error) {
	id, err := s.d.IDs.New(KindCluster)
	if err != nil {
		return nil, fmt.Errorf("new cluster id: %w", err)
	}
	password := cmd.Spec.GetBootstrap().GetUserPassword()
	return s.d.Ops.Submit(ctx, operations.Spec{
		ResourceID: id, Description: "Create PostgreSQL cluster", CreatedBy: reqctx.PrincipalFrom(ctx).ID,
		Request: cmd.Request, Detached: s.d.Config.Detached,
		Prepare: func(ctx context.Context) error { return s.prepareNewCluster(ctx, id, cmd.Metadata, cmd.Spec) },
		Work: func(ctx context.Context) error {
			return s.ensureCluster(ctx, id, run{password: password, firstBackup: true})
		},
	})
}

// prepareNewCluster validates and stores a cluster being created (or
// restored): PROVISIONING, bound to its network.
func (s *Service) prepareNewCluster(ctx context.Context, id string, in *commonpb.ResourceMetadata, spec *pgpb.ClusterSpec) error {
	md, err := s.newMetadata(id, in)
	if err != nil {
		return err
	}
	if err := s.checkProject(ctx, md.GetParentId()); err != nil {
		return err
	}
	if err := s.checkNewSpec(spec); err != nil {
		return err
	}
	stored, _ := stripPassword(spec)
	networkID, err := s.resolveNetwork(ctx, md.GetParentId(), stored.GetNetworkId())
	if err != nil {
		return err
	}
	stored.NetworkId = networkID
	c := &pgpb.Cluster{Metadata: md, Spec: stored, Status: &pgpb.ClusterStatus{
		Phase:         mspv1alpha1pb.ClusterStatus_PHASE_PROVISIONING,
		State:         mspv1alpha1pb.ClusterStatus_STATE_IN_PROGRESS,
		PresetDetails: presetDetails(stored.GetConfig().GetTemplate().GetResources().GetPreset()),
	}}
	return s.storeNewCluster(ctx, c, networkID)
}

// storeNewCluster binds the cluster to its network and stores it.
func (s *Service) storeNewCluster(ctx context.Context, c *pgpb.Cluster, networkID string) error {
	nets, ok := s.d.Peers.Networks()
	if !ok {
		return notEnabled(networkField, "vpc")
	}
	id := c.GetMetadata().GetId()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := nets.Bind(ctx, networkID, id); err != nil {
		return fmt.Errorf("%s: %w", networkField, err)
	}
	if err := s.d.Bindings.Set(ctx, id, networkID); err != nil {
		return errors.Join(err, nets.Unbind(ctx, networkID, id))
	}
	if err := s.d.Clusters.Create(ctx, c); err != nil {
		return errors.Join(err, nets.Unbind(ctx, networkID, id), s.d.Bindings.Delete(ctx, id))
	}
	return nil
}

// resolveNetwork returns the cluster's network: the project's default
// network when networkID is empty.
func (s *Service) resolveNetwork(ctx context.Context, projectID, networkID string) (string, error) {
	if networkID == "" {
		ds, ok := s.d.Peers.DefaultSubnets()
		if !ok {
			return "", notEnabled(networkField, "vpc")
		}
		sn, err := ds.DefaultSubnet(ctx, projectID)
		if errs.IsKind(err, errs.KindNotFound) {
			return "", errs.BadRequest(errs.Violation{Field: networkField, Message: fmt.Sprintf(
				"project %s has no default network: set network_id", projectID)})
		}
		return sn.NetworkID, err
	}
	nets, ok := s.d.Peers.Networks()
	if !ok {
		return "", notEnabled(networkField, "vpc")
	}
	n, err := nets.Network(ctx, networkID)
	if err != nil {
		return "", fmt.Errorf("%s: %w", networkField, err)
	}
	if n.ProjectID != projectID {
		return "", errs.BadRequest(errs.Violation{Field: networkField, Message: fmt.Sprintf(
			"network %s belongs to %s, not to the cluster's project %s", networkID, n.ProjectID, projectID)})
	}
	return n.ID, nil
}

// UpdateCluster patches the mutable parts of a cluster and reconciles its
// hosts (resources, hosts, disk, parameters, pooler, password).
func (s *Service) UpdateCluster(ctx context.Context, cmd UpdateClusterCmd) (*operations.Operation, error) {
	id := cmd.Metadata.GetId()
	password := cmd.Spec.GetBootstrap().GetUserPassword()
	reconcile := false
	return s.d.Ops.Submit(ctx, operations.Spec{
		ResourceID: id, Description: "Update PostgreSQL cluster", CreatedBy: reqctx.PrincipalFrom(ctx).ID,
		Request: cmd.Request, Detached: s.d.Config.Detached,
		Prepare: func(ctx context.Context) error {
			var err error
			reconcile, err = s.prepareUpdate(ctx, cmd)
			return err
		},
		Work: func(ctx context.Context) error {
			if !reconcile {
				return nil
			}
			return s.ensureCluster(ctx, id, run{password: password})
		},
	})
}

// prepareUpdate patches and stores the cluster; it reports whether the
// backend has to reconcile.
func (s *Service) prepareUpdate(ctx context.Context, cmd UpdateClusterCmd) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, err := s.d.Clusters.Get(ctx, cmd.Metadata.GetId())
	if err != nil {
		return false, err
	}
	if err := checkUpdatable(stored, cmd.Metadata); err != nil {
		return false, err
	}
	id := stored.GetMetadata().GetId()
	if stored.GetStatus().GetPhase() == mspv1alpha1pb.ClusterStatus_PHASE_DELETING {
		return false, errs.BadState(id, "the cluster is being deleted")
	}
	if isStopped(stored) {
		return false, errs.BadState(id, fmt.Sprintf("cluster %s is stopped: start it before updating it", stored.GetMetadata().GetName()))
	}
	updated, err := patch(stored, &pgpb.Cluster{
		Metadata: &commonpb.ResourceMetadata{Name: cmd.Metadata.GetName(), Labels: cmd.Metadata.GetLabels()},
		Spec:     cmd.Spec,
	}, cmd.ResetMask)
	if err != nil {
		return false, err
	}
	spec, password := stripPassword(updated.GetSpec())
	updated.Spec = spec
	if err := checkUpdate(stored.GetSpec(), spec); err != nil {
		return false, err
	}
	if err := s.checkSpec(spec); err != nil {
		return false, err
	}
	specChanged := !proto.Equal(spec, stored.GetSpec()) || password != ""
	if !specChanged && proto.Equal(updated, stored) {
		return false, nil
	}
	s.bump(updated.GetMetadata())
	if specChanged {
		updated.Status.Phase = mspv1alpha1pb.ClusterStatus_PHASE_UPDATING
		updated.Status.State = mspv1alpha1pb.ClusterStatus_STATE_IN_PROGRESS
		updated.Status.PresetDetails = presetDetails(spec.GetConfig().GetTemplate().GetResources().GetPreset())
	}
	return specChanged, s.d.Clusters.Update(ctx, updated)
}

// DeleteCluster deletes a cluster, its hosts and its data. Its backups stay
// until their retention expires.
func (s *Service) DeleteCluster(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.d.Ops.Submit(ctx, operations.Spec{
		ResourceID: id, Description: "Delete PostgreSQL cluster", CreatedBy: reqctx.PrincipalFrom(ctx).ID,
		Request: request, Detached: s.d.Config.Detached,
		Prepare: func(ctx context.Context) error {
			return s.transition(ctx, id, func(c *pgpb.Cluster) error {
				setStatus(c, mspv1alpha1pb.ClusterStatus_PHASE_DELETING, mspv1alpha1pb.ClusterStatus_STATE_IN_PROGRESS)
				return nil
			})
		},
		Work: func(ctx context.Context) error { return s.removeCluster(ctx, id) },
	})
}

func (s *Service) removeCluster(ctx context.Context, id string) error {
	unlock := s.clusterLock(id)
	defer s.forgetLock(id)
	defer unlock()
	c, err := s.d.Clusters.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := s.d.Backend.DeleteCluster(ctx, id); err != nil {
		return s.fail(ctx, c, err)
	}
	if err := s.hideBackups(ctx, id); err != nil {
		return err
	}
	if err := s.unbindNetwork(ctx, id); err != nil {
		return err
	}
	return s.d.Clusters.Delete(ctx, id)
}

// unbindNetwork releases the network recorded when the cluster was created.
func (s *Service) unbindNetwork(ctx context.Context, id string) error {
	networkID, err := s.d.Bindings.Get(ctx, id)
	if errs.IsKind(err, errs.KindNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if nets, ok := s.d.Peers.Networks(); ok {
		if err := nets.Unbind(ctx, networkID, id); err != nil {
			return err
		}
	}
	return s.d.Bindings.Delete(ctx, id)
}

// StopCluster stops the hosts of a running cluster, keeping the data.
func (s *Service) StopCluster(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.d.Ops.Submit(ctx, operations.Spec{
		ResourceID: id, Description: "Stop PostgreSQL cluster", CreatedBy: reqctx.PrincipalFrom(ctx).ID,
		Request: request, Detached: s.d.Config.Detached,
		Prepare: func(ctx context.Context) error {
			return s.transition(ctx, id, func(c *pgpb.Cluster) error {
				if c.GetStatus().GetPhase() != mspv1alpha1pb.ClusterStatus_PHASE_RUNNING {
					return errs.BadState(id, fmt.Sprintf("cluster %s is not running (phase %s)", c.GetMetadata().GetName(), c.GetStatus().GetPhase()))
				}
				setStatus(c, mspv1alpha1pb.ClusterStatus_PHASE_STOPPING, mspv1alpha1pb.ClusterStatus_STATE_IN_PROGRESS)
				return nil
			})
		},
		Work: func(ctx context.Context) error { return s.stopCluster(ctx, id) },
	})
}

func (s *Service) stopCluster(ctx context.Context, id string) error {
	unlock := s.clusterLock(id)
	defer unlock()
	c, err := s.d.Clusters.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := s.d.Backend.StopCluster(ctx, id); err != nil {
		return s.fail(ctx, c, err)
	}
	c.Status.State = mspv1alpha1pb.ClusterStatus_STATE_FINISHED
	return s.d.Clusters.Update(ctx, c)
}

// StartCluster starts a stopped cluster again.
func (s *Service) StartCluster(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.d.Ops.Submit(ctx, operations.Spec{
		ResourceID: id, Description: "Start PostgreSQL cluster", CreatedBy: reqctx.PrincipalFrom(ctx).ID,
		Request: request, Detached: s.d.Config.Detached,
		Prepare: func(ctx context.Context) error {
			return s.transition(ctx, id, func(c *pgpb.Cluster) error {
				if !isStopped(c) {
					return errs.BadState(id, fmt.Sprintf("cluster %s is not stopped", c.GetMetadata().GetName()))
				}
				setStatus(c, mspv1alpha1pb.ClusterStatus_PHASE_RESUMING, mspv1alpha1pb.ClusterStatus_STATE_IN_PROGRESS)
				return nil
			})
		},
		Work: func(ctx context.Context) error { return s.ensureCluster(ctx, id, run{}) },
	})
}

// transition applies fn to the stored cluster under the Prepare lock and
// stores the result.
func (s *Service) transition(ctx context.Context, id string, fn func(*pgpb.Cluster) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.d.Clusters.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := fn(c); err != nil {
		return err
	}
	return s.d.Clusters.Update(ctx, c)
}

func setStatus(c *pgpb.Cluster, phase mspv1alpha1pb.ClusterStatus_Phase, state mspv1alpha1pb.ClusterStatus_State) {
	if c.GetStatus() == nil {
		c.Status = &pgpb.ClusterStatus{}
	}
	c.Status.Phase, c.Status.State = phase, state
}

// isStopped reports whether the cluster is stopped: the API has no STOPPED
// phase, a stopped cluster is STOPPING and FINISHED.
func isStopped(c *pgpb.Cluster) bool {
	return c.GetStatus().GetPhase() == mspv1alpha1pb.ClusterStatus_PHASE_STOPPING &&
		c.GetStatus().GetState() == mspv1alpha1pb.ClusterStatus_STATE_FINISHED
}

// isRunning reports whether the cluster is up and idle. A cluster whose disk
// is full (STATE_DEGRADED) still runs: it serves reads and takes backups.
func isRunning(c *pgpb.Cluster) bool {
	return c.GetStatus().GetPhase() == mspv1alpha1pb.ClusterStatus_PHASE_RUNNING &&
		(c.GetStatus().GetState() == mspv1alpha1pb.ClusterStatus_STATE_FINISHED ||
			c.GetStatus().GetState() == mspv1alpha1pb.ClusterStatus_STATE_DEGRADED)
}

// fail records a backend failure in the cluster's state and returns it.
func (s *Service) fail(ctx context.Context, c *pgpb.Cluster, err error) error {
	c.Status.State = mspv1alpha1pb.ClusterStatus_STATE_ERROR
	return errors.Join(err, s.d.Clusters.Update(ctx, c))
}
