package domain

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mlflowpb "github.com/nebius/gosdk/proto/nebius/msp/mlflow/v1alpha1"
	mspv1alpha1pb "github.com/nebius/gosdk/proto/nebius/msp/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// maxAdminUsername is the longest spec.admin_username (MLflow user names);
// spec.admin_password must be longer than minAdminPassword (MLflow basic auth).
const (
	maxAdminUsername = 63
	minAdminPassword = 12
)

// Shorthands for the phase and state enums.
const (
	phaseProvisioning = mspv1alpha1pb.ClusterStatus_PHASE_PROVISIONING
	phaseRunning      = mspv1alpha1pb.ClusterStatus_PHASE_RUNNING
	phaseDeleting     = mspv1alpha1pb.ClusterStatus_PHASE_DELETING
	phaseStopping     = mspv1alpha1pb.ClusterStatus_PHASE_STOPPING
	phaseResuming     = mspv1alpha1pb.ClusterStatus_PHASE_RESUMING

	stateInProgress = mspv1alpha1pb.ClusterStatus_STATE_IN_PROGRESS
	stateFinished   = mspv1alpha1pb.ClusterStatus_STATE_FINISHED
	stateError      = mspv1alpha1pb.ClusterStatus_STATE_ERROR
)

// CreateClusterCmd is the input of Create. Request is kept (without the
// admin password) as the operation's request.
type CreateClusterCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *mlflowpb.ClusterSpec
	Request  *mlflowpb.CreateClusterRequest
}

// Get returns a cluster.
func (s *Service) Get(ctx context.Context, id string) (*mlflowpb.Cluster, error) {
	c, err := s.d.Clusters.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.withExperiments(ctx, c), nil
}

// GetByName returns the cluster with the name in a project.
func (s *Service) GetByName(ctx context.Context, parentID, name string) (*mlflowpb.Cluster, error) {
	c, err := s.d.Clusters.GetByName(ctx, parentID, name)
	if err != nil {
		return nil, err
	}
	return s.withExperiments(ctx, c), nil
}

// List returns one page of a project's clusters in creation order.
func (s *Service) List(ctx context.Context, parentID string, size int64, pageToken string) (Page[*mlflowpb.Cluster], error) {
	n, err := pageSize(size)
	if err != nil {
		return Page[*mlflowpb.Cluster]{}, err
	}
	page, err := s.d.Clusters.List(ctx, parentID, n, pageToken)
	if err != nil {
		return Page[*mlflowpb.Cluster]{}, err
	}
	for i, c := range page.Items {
		page.Items[i] = s.withExperiments(ctx, c)
	}
	return page, nil
}

// withExperiments fills status.experiments_count of a running cluster from
// its backend; when the backend does not answer in time the stored value
// stays.
func (s *Service) withExperiments(ctx context.Context, c *mlflowpb.Cluster) *mlflowpb.Cluster {
	st := c.GetStatus()
	if st.GetPhase() != phaseRunning || st.GetState() != stateFinished {
		return c
	}
	ctx, cancel := context.WithTimeout(ctx, experimentsTimeout)
	defer cancel()
	n, err := s.d.Backend.ExperimentsCount(ctx, c.GetMetadata().GetId())
	if err != nil {
		s.log.WarnContext(ctx, "mlflow experiments count failed", "cluster", c.GetMetadata().GetId(), "error", err)
		return c
	}
	st.ExperimentsCount = uint32(max(0, min(n, math.MaxUint32)))
	return c
}

// Create creates a cluster: PROVISIONING until the tracking server answers,
// then RUNNING with its endpoints. The artifact bucket and the service
// account's access key are created by the operation.
func (s *Service) Create(ctx context.Context, cmd CreateClusterCmd) (*operations.Operation, error) {
	id, err := s.d.IDs.New(KindCluster)
	if err != nil {
		return nil, fmt.Errorf("new cluster id: %w", err)
	}
	password := cmd.Spec.GetAdminPassword()
	return s.d.Ops.Submit(ctx, operations.Spec{
		ResourceID: id, Description: "Create cluster", CreatedBy: reqctx.PrincipalFrom(ctx).ID,
		Request: RedactRequest(cmd.Request), Detached: s.d.Config.Detached,
		Prepare: func(ctx context.Context) error { return s.prepareCreate(ctx, id, cmd) },
		Work: func(ctx context.Context) error {
			return s.run(ctx, id, func(ctx context.Context) error { return s.ensure(ctx, id, password) })
		},
	})
}

func (s *Service) prepareCreate(ctx context.Context, id string, cmd CreateClusterCmd) error {
	md, err := s.newMetadata(id, cmd.Metadata)
	if err != nil {
		return err
	}
	if err := s.checkProject(ctx, md.GetParentId()); err != nil {
		return err
	}
	spec := proto.CloneOf(cmd.Spec)
	if spec == nil {
		spec = &mlflowpb.ClusterSpec{}
	}
	if err := s.checkSpec(ctx, md.GetParentId(), spec); err != nil {
		return err
	}
	if err := s.checkPeers(); err != nil {
		return err
	}
	networkID, err := s.resolveNetwork(ctx, md.GetParentId(), spec.GetNetworkId())
	if err != nil {
		return err
	}
	// The password goes to the backend from the request only.
	spec.AdminPassword = ""
	spec.NetworkId = networkID
	if spec.GetSize() == "" {
		spec.Size = s.d.Config.SizeOrder[0]
	}
	c := &mlflowpb.Cluster{Metadata: md, Spec: spec, Status: &mlflowpb.MlflowClusterStatus{
		Phase: phaseProvisioning, State: stateInProgress, MlflowVersion: s.d.Config.MLflowVersion,
		EffectiveStorageBucketName: spec.GetStorageBucketName(),
	}}
	return s.storeNew(ctx, c, networkID)
}

// checkSpec validates the request spec of a new cluster.
func (s *Service) checkSpec(ctx context.Context, projectID string, spec *mlflowpb.ClusterSpec) error {
	var v []errs.Violation
	switch n := utf8.RuneCountInString(spec.GetAdminUsername()); {
	case n == 0:
		v = append(v, errs.Violation{Field: "spec.admin_username", Message: "required"})
	case n > maxAdminUsername:
		v = append(v, errs.Violation{Field: "spec.admin_username", Message: fmt.Sprintf("must be at most %d characters", maxAdminUsername)})
	}
	// MLflow's basic-auth app refuses admin passwords of 12 characters or
	// fewer, so such a cluster could never start.
	switch n := utf8.RuneCountInString(spec.GetAdminPassword()); {
	case n == 0:
		v = append(v, errs.Violation{Field: "spec.admin_password", Message: "required"})
	case n <= minAdminPassword:
		v = append(v, errs.Violation{Field: "spec.admin_password", Message: fmt.Sprintf("must be longer than %d characters", minAdminPassword)})
	}
	if sz := spec.GetSize(); sz != "" {
		if _, ok := s.d.Config.Sizes[sz]; !ok {
			v = append(v, errs.Violation{Field: "spec.size", Message: fmt.Sprintf("unknown size %q: use one of %s", sz, strings.Join(s.d.Config.SizeOrder, ", "))})
		}
	}
	if sa := spec.GetServiceAccountId(); sa == "" {
		v = append(v, errs.Violation{Field: "spec.service_account_id", Message: "required"})
	} else if !strings.HasPrefix(sa, KindServiceAccount+"-") {
		v = append(v, errs.Violation{Field: "spec.service_account_id", Message: "must be a service account ID"})
	}
	if len(v) > 0 {
		return errs.BadRequest(v...)
	}
	return s.checkServiceAccount(ctx, projectID, spec.GetServiceAccountId())
}

// checkServiceAccount requires an existing service account of the cluster's
// project.
func (s *Service) checkServiceAccount(ctx context.Context, projectID, saID string) error {
	const field = "spec.service_account_id"
	parent, err := s.d.Hierarchy.Parent(ctx, saID)
	if err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	if parent != projectID {
		return errs.BadRequest(errs.Violation{Field: field, Message: fmt.Sprintf(
			"service account %s belongs to %s, not to the cluster's project %s", saID, parent, projectID)})
	}
	return nil
}

// checkPeers requires the capabilities creating a cluster uses, and that
// a named artifact bucket exists.
func (s *Service) checkPeers() error {
	if _, ok := s.d.Peers.Buckets(); !ok {
		return notEnabled("spec.storage_bucket_name", "storage")
	}
	if _, ok := s.d.Peers.SystemAccessKeys(); !ok {
		return notEnabled("spec.service_account_id", "iam")
	}
	if _, ok := s.d.Peers.ProjectRegions(); !ok {
		return notEnabled("metadata.parent_id", "iam")
	}
	return nil
}

// resolveNetwork returns the cluster's network: the one set, or the
// project's default network.
func (s *Service) resolveNetwork(ctx context.Context, projectID, networkID string) (string, error) {
	const field = "spec.network_id"
	nets, ok := s.d.Peers.Networks()
	if !ok {
		return "", notEnabled(field, "vpc")
	}
	if networkID == "" {
		ds, ok := s.d.Peers.DefaultSubnets()
		if !ok {
			return "", notEnabled(field, "vpc")
		}
		sn, err := ds.DefaultSubnet(ctx, projectID)
		if errs.IsKind(err, errs.KindNotFound) {
			return "", errs.BadRequest(errs.Violation{Field: field, Message: fmt.Sprintf(
				"project %s has no default network: set network_id", projectID)})
		}
		if err != nil {
			return "", err
		}
		return sn.NetworkID, nil
	}
	n, err := nets.Network(ctx, networkID)
	if err != nil {
		return "", fmt.Errorf("%s: %w", field, err)
	}
	if n.ProjectID != projectID {
		return "", errs.BadRequest(errs.Violation{Field: field, Message: fmt.Sprintf(
			"network %s belongs to %s, not to the cluster's project %s", networkID, n.ProjectID, projectID)})
	}
	return networkID, nil
}

// storeNew checks the named bucket, binds the cluster to its network and
// stores it.
func (s *Service) storeNew(ctx context.Context, c *mlflowpb.Cluster, networkID string) error {
	const bucketField = "spec.storage_bucket_name"
	projectID, id := c.GetMetadata().GetParentId(), c.GetMetadata().GetId()
	if name := c.GetSpec().GetStorageBucketName(); name != "" {
		buckets, _ := s.d.Peers.Buckets()
		if _, err := buckets.BucketByName(ctx, projectID, name); err != nil {
			return fmt.Errorf("%s: bucket %q: %w", bucketField, name, err)
		}
	}
	nets, ok := s.d.Peers.Networks()
	if !ok {
		return notEnabled("spec.network_id", "vpc")
	}
	if err := nets.Bind(ctx, networkID, id); err != nil {
		return fmt.Errorf("spec.network_id: %w", err)
	}
	if err := s.d.Internals.Put(ctx, id, Internals{NetworkID: networkID}); err != nil {
		return errors.Join(err, nets.Unbind(ctx, networkID, id))
	}
	if err := s.d.Clusters.Create(ctx, c); err != nil {
		return errors.Join(err, nets.Unbind(ctx, networkID, id), s.d.Internals.Delete(ctx, id))
	}
	return nil
}

// run executes backend work and records a failure in the cluster's state
// (the phase stays as it was).
func (s *Service) run(ctx context.Context, id string, work func(context.Context) error) error {
	err := work(ctx)
	if err == nil {
		return nil
	}
	c, gerr := s.d.Clusters.Get(ctx, id)
	if gerr != nil {
		return errors.Join(err, gerr)
	}
	c.Status.State = stateError
	s.bump(c.GetMetadata())
	return errors.Join(err, s.d.Clusters.Update(ctx, c))
}

// ensure brings the tracking server up (create, or start of a stopped
// one): artifact bucket, access key, backend, then status.
func (s *Service) ensure(ctx context.Context, id, password string) error {
	c, err := s.d.Clusters.Get(ctx, id)
	if err != nil {
		return err
	}
	in, err := s.d.Internals.Get(ctx, id)
	if err != nil {
		return err
	}
	bucket, err := s.ensureBucket(ctx, c)
	if err != nil {
		return err
	}
	if in, err = s.ensureAccessKey(ctx, c, in); err != nil {
		return err
	}
	region, err := s.region(ctx, c.GetMetadata().GetParentId())
	if err != nil {
		return err
	}
	cpus, mem, _ := parseSize(s.d.Config.Sizes[c.GetSpec().GetSize()])
	ep, err := s.d.Backend.EnsureCluster(ctx, BackendCluster{
		ID: id, ProjectID: c.GetMetadata().GetParentId(), Name: c.GetMetadata().GetName(),
		AdminUsername: c.GetSpec().GetAdminUsername(), AdminPassword: password, PublicAccess: c.GetSpec().GetPublicAccess(),
		CPUs: cpus, MemoryBytes: mem << 30,
		Artifacts: ArtifactStore{Bucket: bucket, Region: region, AccessKeyID: in.AWSAccessKeyID, Secret: in.Secret},
	})
	if err != nil {
		return err
	}
	c.Status = &mlflowpb.MlflowClusterStatus{
		Phase: phaseRunning, State: stateFinished, MlflowVersion: s.d.Config.MLflowVersion,
		EffectiveStorageBucketName: bucket, ExperimentsCount: c.GetStatus().GetExperimentsCount(),
		TrackingEndpoints: &mlflowpb.Endpoints{Private: ep.Private, Public: ep.Public},
		TrackingEndpoint:  ep.Private,
	}
	if c.GetSpec().GetPublicAccess() && ep.Public != "" {
		c.Status.TrackingEndpoint = ep.Public
	}
	s.bump(c.GetMetadata())
	return s.d.Clusters.Update(ctx, c)
}

// ensureBucket returns the artifact bucket: the named one, else the
// cluster's own, created on first use (and reused when it already exists).
func (s *Service) ensureBucket(ctx context.Context, c *mlflowpb.Cluster) (string, error) {
	if name := c.GetStatus().GetEffectiveStorageBucketName(); name != "" {
		return name, nil
	}
	buckets, ok := s.d.Peers.Buckets()
	if !ok {
		return "", notEnabled("spec.storage_bucket_name", "storage")
	}
	project, name := c.GetMetadata().GetParentId(), c.GetMetadata().GetId()
	b, err := buckets.CreateBucket(ctx, project, name)
	if errs.IsKind(err, errs.KindAlreadyExists) {
		b, err = buckets.BucketByName(ctx, project, name)
	}
	if err != nil {
		return "", fmt.Errorf("artifact bucket %q: %w", name, err)
	}
	return b.Name, nil
}

// ensureAccessKey creates the service account's S3 key on first use and
// records it.
func (s *Service) ensureAccessKey(ctx context.Context, c *mlflowpb.Cluster, in Internals) (Internals, error) {
	if in.AccessKeyID != "" {
		return in, nil
	}
	keys, ok := s.d.Peers.SystemAccessKeys()
	if !ok {
		return in, notEnabled("spec.service_account_id", "iam")
	}
	k, err := keys.CreateSystemAccessKey(ctx, peer.SystemAccessKeySpec{
		ParentID: c.GetMetadata().GetParentId(), Name: c.GetMetadata().GetId(),
		Description: fmt.Sprintf("Managed MLflow cluster %s artifacts", c.GetMetadata().GetName()),
		AccountID:   c.GetSpec().GetServiceAccountId(),
	})
	if err != nil {
		return in, fmt.Errorf("artifact access key: %w", err)
	}
	in.AccessKeyID, in.AWSAccessKeyID, in.Secret = k.KeyID, k.AccessKeyID, k.Secret
	if err := s.d.Internals.Put(ctx, c.GetMetadata().GetId(), in); err != nil {
		return in, errors.Join(err, keys.DeleteSystemAccessKey(ctx, k.KeyID))
	}
	return in, nil
}

func (s *Service) region(ctx context.Context, projectID string) (string, error) {
	regions, ok := s.d.Peers.ProjectRegions()
	if !ok {
		return "", notEnabled("metadata.parent_id", "iam")
	}
	return regions.Region(ctx, projectID)
}

// Stop stops a running cluster, keeping its data: STOPPING, which is
// finished once the server is down.
func (s *Service) Stop(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.d.Ops.Submit(ctx, operations.Spec{
		ResourceID: id, Description: "Stop cluster", CreatedBy: reqctx.PrincipalFrom(ctx).ID,
		Request: request, Detached: s.d.Config.Detached,
		Prepare: func(ctx context.Context) error {
			return s.transition(ctx, id, "stopped", "only a running cluster can be stopped", canStop, phaseStopping)
		},
		Work: func(ctx context.Context) error {
			return s.run(ctx, id, func(ctx context.Context) error {
				if err := s.d.Backend.StopCluster(ctx, id); err != nil {
					return err
				}
				return s.finish(ctx, id, phaseStopping)
			})
		},
	})
}

// Start starts a stopped cluster again.
func (s *Service) Start(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.d.Ops.Submit(ctx, operations.Spec{
		ResourceID: id, Description: "Start cluster", CreatedBy: reqctx.PrincipalFrom(ctx).ID,
		Request: request, Detached: s.d.Config.Detached,
		Prepare: func(ctx context.Context) error {
			return s.transition(ctx, id, "started", "only a stopped cluster can be started", canStart, phaseResuming)
		},
		Work: func(ctx context.Context) error {
			return s.run(ctx, id, func(ctx context.Context) error { return s.ensure(ctx, id, "") })
		},
	})
}

// canStop reports whether a cluster in this phase and state may be stopped:
// running, or stopping whose stop failed.
func canStop(phase mspv1alpha1pb.ClusterStatus_Phase, state mspv1alpha1pb.ClusterStatus_State) bool {
	return (phase == phaseRunning && state == stateFinished) || (phase == phaseStopping && state == stateError)
}

// canStart reports whether a cluster may be started: stopped (STOPPING and
// finished), or one whose stop or start failed.
func canStart(phase mspv1alpha1pb.ClusterStatus_Phase, state mspv1alpha1pb.ClusterStatus_State) bool {
	return (phase == phaseStopping && (state == stateFinished || state == stateError)) ||
		(phase == phaseResuming && state == stateError)
}

// transition moves the cluster to phase (IN_PROGRESS) if allowed.
func (s *Service) transition(ctx context.Context, id, verb, rule string,
	allowed func(mspv1alpha1pb.ClusterStatus_Phase, mspv1alpha1pb.ClusterStatus_State) bool, to mspv1alpha1pb.ClusterStatus_Phase,
) error {
	c, err := s.d.Clusters.Get(ctx, id)
	if err != nil {
		return err
	}
	st := c.GetStatus()
	if !allowed(st.GetPhase(), st.GetState()) {
		return errs.BadState(id, fmt.Sprintf("cluster %s cannot be %s: it is %s/%s; %s", c.GetMetadata().GetName(), verb,
			st.GetPhase(), st.GetState(), rule))
	}
	st.Phase, st.State = to, stateInProgress
	s.bump(c.GetMetadata())
	return s.d.Clusters.Update(ctx, c)
}

// finish marks the phase's work done.
func (s *Service) finish(ctx context.Context, id string, phase mspv1alpha1pb.ClusterStatus_Phase) error {
	c, err := s.d.Clusters.Get(ctx, id)
	if err != nil {
		return err
	}
	c.Status.Phase, c.Status.State = phase, stateFinished
	s.bump(c.GetMetadata())
	return s.d.Clusters.Update(ctx, c)
}

// Delete deletes a cluster: its server and data, its access key and its
// network binding. The artifact bucket stays.
func (s *Service) Delete(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.d.Ops.Submit(ctx, operations.Spec{
		ResourceID: id, Description: "Delete cluster", CreatedBy: reqctx.PrincipalFrom(ctx).ID,
		Request: request, Detached: s.d.Config.Detached,
		Prepare: func(ctx context.Context) error {
			c, err := s.d.Clusters.Get(ctx, id)
			if err != nil {
				return err
			}
			c.Status.Phase, c.Status.State = phaseDeleting, stateInProgress
			s.bump(c.GetMetadata())
			return s.d.Clusters.Update(ctx, c)
		},
		Work: func(ctx context.Context) error {
			return s.run(ctx, id, func(ctx context.Context) error { return s.remove(ctx, id) })
		},
	})
}

func (s *Service) remove(ctx context.Context, id string) error {
	if err := s.d.Backend.DeleteCluster(ctx, id); err != nil {
		return err
	}
	in, err := s.d.Internals.Get(ctx, id)
	if err != nil && !errs.IsKind(err, errs.KindNotFound) {
		return err
	}
	if keys, ok := s.d.Peers.SystemAccessKeys(); ok && in.AccessKeyID != "" {
		if err := keys.DeleteSystemAccessKey(ctx, in.AccessKeyID); err != nil {
			return fmt.Errorf("delete the artifact access key: %w", err)
		}
	}
	if nets, ok := s.d.Peers.Networks(); ok && in.NetworkID != "" {
		if err := nets.Unbind(ctx, in.NetworkID, id); err != nil {
			return err
		}
	}
	if err := s.d.Internals.Delete(ctx, id); err != nil {
		return err
	}
	return s.d.Clusters.Delete(ctx, id)
}

// parseSize splits a size's "<n>vcpu-<m>gb" resources.
func parseSize(preset string) (cpus, memoryGiB int64, ok bool) {
	return config.ParsePreset(preset)
}
