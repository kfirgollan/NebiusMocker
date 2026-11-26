package domain

import (
	"context"
	"errors"
	"fmt"

	pgpb "github.com/nebius/gosdk/proto/nebius/msp/postgresql/v1alpha1"
	mspv1alpha1pb "github.com/nebius/gosdk/proto/nebius/msp/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// diskLimit is what a cluster's disk holds: size_gibibytes times the
// configured bytes per GiB (NEBIUS_MOCKER_MSP_POSTGRESQL_DISK_UNIT).
func (s *Service) diskLimit(c *pgpb.Cluster) int64 {
	unit := s.d.Config.MSP.PostgresDiskUnit
	if unit <= 0 {
		unit = gib
	}
	return c.GetSpec().GetConfig().GetTemplate().GetDisk().GetSizeGibibytes() * unit
}

// diskState checks the disk of a cluster the backend has just reconciled and
// returns its state: DEGRADED while the disk is full, else FINISHED. A check
// that fails is logged and does not fail the operation: the next periodic
// check catches up.
func (s *Service) diskState(ctx context.Context, c *pgpb.Cluster) mspv1alpha1pb.ClusterStatus_State {
	usage, err := s.d.Backend.CheckDisk(ctx, c.GetMetadata().GetId(), s.diskLimit(c))
	if err != nil {
		s.log.WarnContext(ctx, "postgresql disk check", "cluster", c.GetMetadata().GetId(), "error", err)
		return mspv1alpha1pb.ClusterStatus_STATE_FINISHED
	}
	return stateOf(usage)
}

func stateOf(u DiskUsage) mspv1alpha1pb.ClusterStatus_State {
	if u.Full {
		return mspv1alpha1pb.ClusterStatus_STATE_DEGRADED
	}
	return mspv1alpha1pb.ClusterStatus_STATE_FINISHED
}

// CheckDisks measures the disk of every running cluster and sets its state
// to DEGRADED while the disk is full and back to FINISHED once it is not.
// The backend call runs outside any lock; the state is stored only if the
// cluster is still running and idle, so a check never overwrites an operation.
// The module runs it every NEBIUS_MOCKER_MSP_POSTGRESQL_DISK_CHECK_INTERVAL.
func (s *Service) CheckDisks(ctx context.Context) error {
	clusters, err := s.d.Clusters.All(ctx)
	if err != nil {
		return err
	}
	var errList []error
	for _, c := range clusters {
		if !isRunning(c) {
			continue
		}
		id := c.GetMetadata().GetId()
		usage, err := s.d.Backend.CheckDisk(ctx, id, s.diskLimit(c))
		if err != nil {
			errList = append(errList, fmt.Errorf("check disk of %s: %w", id, err))
			continue
		}
		version := c.GetMetadata().GetResourceVersion()
		err = s.transition(ctx, id, func(cur *pgpb.Cluster) error {
			// A spec change meanwhile (a grown disk) makes this measurement's
			// limit stale; the update's own check has the current one.
			if isRunning(cur) && cur.GetMetadata().GetResourceVersion() == version {
				cur.Status.State = stateOf(usage)
			}
			return nil
		})
		if err != nil && !errs.IsKind(err, errs.KindNotFound) { // deleted meanwhile
			errList = append(errList, fmt.Errorf("store disk state of %s: %w", id, err))
		}
	}
	return errors.Join(errList...)
}
