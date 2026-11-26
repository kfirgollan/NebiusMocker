package docker

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/mspmlflow/domain"
)

const (
	tlsCertPath = "/etc/nmock/tls/tls.crt"
	tlsKeyPath  = "/etc/nmock/tls/tls.key"
	authPath    = "/etc/nmock/auth.ini"
	dataDir     = "/mlflow"
	serverCmd   = `exec mlflow server --app-name basic-auth --host 0.0.0.0 --port 5000 --workers 2 ` +
		`--backend-store-uri sqlite:////mlflow/mlflow.db --artifacts-destination "s3://$1/" --serve-artifacts ` +
		`--gunicorn-opts "--certfile=` + tlsCertPath + ` --keyfile=` + tlsKeyPath + `"`
)

// Names carry the owner tag: concurrent mockers on one Docker host allocate
// the same sequential cluster IDs, and their objects must not collide.
func (b *Backend) containerName(id string) string {
	return "nmock-mlflow-" + dockerhost.OwnerTag(b.owner) + "-" + id
}

func (b *Backend) volumeName(id string) string { return b.containerName(id) }

// s3Endpoint is where the servers reach the mocker's S3 front.
func s3Endpoint(cfg config.MSPConfig, listen string) (string, error) {
	if cfg.S3Endpoint != "" {
		return cfg.S3Endpoint, nil
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" || port == "0" {
		return "", fmt.Errorf("msp mlflow docker backend: cannot derive the S3 endpoint from the S3 front address %q; set NEBIUS_MOCKER_MSP_S3_ENDPOINT", listen)
	}
	return "http://" + net.JoinHostPort("host.docker.internal", port), nil
}

// ensureImage builds the server image on first use.
func (b *Backend) ensureImage(ctx context.Context) error {
	b.buildMu.Lock()
	defer b.buildMu.Unlock()
	ok, err := b.docker.ImageExists(ctx, b.image)
	if err != nil {
		return internal("look up image "+b.image, err)
	}
	if ok {
		return nil
	}
	if err := b.docker.EnsureImage(ctx, b.cfg.MLflowImage); err != nil {
		return internal("pull image "+b.cfg.MLflowImage, err)
	}
	b.log.InfoContext(ctx, "msp mlflow: building the server image (once, about a minute)", "image", b.image)
	dockerfile := fmt.Sprintf("FROM %s\nRUN pip install --no-cache-dir boto3 \"mlflow[auth]==%s\"\n", b.cfg.MLflowImage, b.cfg.MLflowVersion)
	if err := b.docker.BuildImage(ctx, b.image, dockerfile, map[string]string{LabelBackend: backendLabel}); err != nil {
		return internal("build image "+b.image, err)
	}
	return nil
}

// EnsureCluster implements domain.Backend.
func (b *Backend) EnsureCluster(ctx context.Context, c domain.BackendCluster) (domain.Endpoints, error) {
	if err := b.ensureImage(ctx); err != nil {
		return domain.Endpoints{}, err
	}
	b.remember(c)
	creds := b.credentialsOf(c.ID)
	if err := b.docker.EnsureVolume(ctx, b.volumeName(c.ID), b.clusterLabels(c.ID)); err != nil {
		return domain.Endpoints{}, internal("create volume", err)
	}
	spec, hash := b.spec(ctx, c, creds)
	id, err := b.converge(ctx, spec, hash)
	if err != nil {
		return domain.Endpoints{}, err
	}
	port, err := b.publishedPort(ctx, id)
	if err != nil {
		return domain.Endpoints{}, err
	}
	if err := b.waitReady(ctx, id, spec.Name, port, creds); err != nil {
		return domain.Endpoints{}, err
	}
	e := domain.Endpoints{Private: net.JoinHostPort(b.cfg.EndpointHost, port)}
	if c.PublicAccess {
		e.Public = e.Private
	}
	return e, nil
}

func (b *Backend) remember(c domain.BackendCluster) {
	if c.AdminPassword == "" {
		return
	}
	b.adminsMu.Lock()
	defer b.adminsMu.Unlock()
	b.admins[c.ID] = credentials{user: c.AdminUsername, password: c.AdminPassword}
}

func (b *Backend) credentialsOf(id string) credentials {
	b.adminsMu.Lock()
	defer b.adminsMu.Unlock()
	return b.admins[id]
}

// spec is the container of the cluster, without its certificate (added at
// creation), and the hash that says when to recreate it.
func (b *Backend) spec(ctx context.Context, c domain.BackendCluster, creds credentials) (spec dockerhost.ContainerSpec, hash string) {
	nano := c.CPUs * 1_000_000_000
	if info, err := b.docker.Info(ctx); err == nil && info.NCPU > 0 {
		nano = min(nano, int64(info.NCPU)*1_000_000_000)
	}
	env := []string{
		"MLFLOW_AUTH_CONFIG_PATH=" + authPath,
		"MLFLOW_FLASK_SERVER_SECRET_KEY=" + b.mac("flask/"+c.ID),
		"MLFLOW_S3_ENDPOINT_URL=" + b.s3,
		"AWS_ACCESS_KEY_ID=" + c.Artifacts.AccessKeyID,
		"AWS_SECRET_ACCESS_KEY=" + c.Artifacts.Secret,
		"AWS_DEFAULT_REGION=" + c.Artifacts.Region,
	}
	cmd := []string{"sh", "-c", serverCmd, "nmock-mlflow", c.Artifacts.Bucket}
	labels := b.clusterLabels(c.ID)
	spec = dockerhost.ContainerSpec{
		Name: b.containerName(c.ID), Image: b.image, Labels: labels, Env: env, Cmd: cmd,
		ExtraHosts: []string{"host.docker.internal:host-gateway"},
		Ports:      []dockerhost.PortBinding{{ContainerPort: serverPort, HostIP: b.cfg.BindIP}},
		Memory:     c.MemoryBytes, NanoCPUs: nano,
		Binds: []string{b.volumeName(c.ID) + ":" + dataDir},
		Files: []dockerhost.File{{Path: authPath, Mode: 0o600, Content: []byte(authINI(c, creds))}},
	}
	sum := sha256.Sum256([]byte(strings.Join(append(slices.Clone(env), cmd[4], b.image, b.cfg.EndpointHost,
		fmt.Sprint(c.MemoryBytes, nano)), "\x00")))
	return spec, hex.EncodeToString(sum[:8])
}

// authINI is MLflow's basic-auth configuration. MLflow creates the admin
// user from it when it first creates the auth database in the volume.
func authINI(c domain.BackendCluster, creds credentials) string {
	user, password := c.AdminUsername, c.AdminPassword
	if password == "" {
		user, password = creds.user, creds.password
	}
	return fmt.Sprintf(`[mlflow]
default_permission = READ
database_uri = sqlite:////mlflow/basic_auth.db
admin_username = %s
admin_password = %s
authorization_function = mlflow.server.auth:authenticate_request_basic_auth
`, user, password)
}

// converge creates the container, starts a stopped one, or recreates it
// when its hash changed (keeping the volume and the pinned port).
func (b *Backend) converge(ctx context.Context, spec dockerhost.ContainerSpec, hash string) (string, error) {
	cur, err := b.docker.InspectContainer(ctx, spec.Name)
	pinned := ""
	switch {
	case err == nil && cur.Labels[labelSpec] == hash:
		if !cur.Running {
			if err := b.docker.StartContainer(ctx, cur.ID); err != nil {
				return "", internal("start "+spec.Name, err)
			}
		}
		return cur.ID, nil
	case err == nil:
		pinned = cur.Labels[labelPort]
		b.log.InfoContext(ctx, "msp mlflow: recreating container, its spec changed", "container", spec.Name)
		if err := b.docker.RemoveContainer(ctx, cur.ID); err != nil {
			return "", internal("remove "+spec.Name, err)
		}
	case !errors.Is(err, dockerhost.ErrNotFound):
		return "", internal("inspect "+spec.Name, err)
	}
	if pinned == "" {
		if pinned, err = dockerhost.FreePort(ctx, b.cfg.BindIP); err != nil {
			return "", internal("pick a host port", err)
		}
	}
	spec.Labels[labelSpec], spec.Labels[labelPort] = hash, pinned
	spec.Ports[0].HostPort = pinned
	cert, err := b.ca.Issue(b.cfg.EndpointHost, "localhost", "127.0.0.1", "host.docker.internal")
	if err != nil {
		return "", internal("issue server certificate", err)
	}
	var chain []byte
	for _, der := range cert.Certificate {
		chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		return "", internal("encode server key", err)
	}
	spec.Files = append(spec.Files,
		dockerhost.File{Path: tlsCertPath, Mode: 0o644, Content: chain},
		dockerhost.File{Path: tlsKeyPath, Mode: 0o600, Content: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})})
	id, err := b.docker.CreateContainer(ctx, spec)
	if err != nil {
		return "", internal("create "+spec.Name, err)
	}
	if err := b.docker.StartContainer(ctx, id); err != nil {
		return "", internal("start "+spec.Name, err)
	}
	return id, nil
}

func (b *Backend) publishedPort(ctx context.Context, id string) (string, error) {
	ctr, err := b.docker.InspectContainer(ctx, id)
	if err != nil {
		return "", internal("inspect "+id, err)
	}
	for _, pb := range ctr.Ports[serverPort] {
		if pb.HostPort != "" {
			return pb.HostPort, nil
		}
	}
	return "", errs.Internal(fmt.Sprintf("container %s publishes no %s port", ctr.Name, serverPort))
}

// adminHost is where the mocker reaches the published ports.
func (b *Backend) adminHost() string {
	if ip := net.ParseIP(b.cfg.BindIP); ip == nil || ip.IsUnspecified() {
		return "127.0.0.1"
	}
	return b.cfg.BindIP
}

// waitReady polls until the server answers the experiments API with the
// admin credentials (or, when they are unknown, with 401: it is up).
func (b *Backend) waitReady(ctx context.Context, id, name, port string, creds credentials) error {
	deadline := b.clk.Now().Add(b.cfg.StartTimeout)
	for {
		ctr, err := b.docker.InspectContainer(ctx, id)
		if err != nil {
			return internal("inspect "+name, err)
		}
		if !ctr.Running {
			return b.startFailure(ctx, id, name, fmt.Sprintf("exited (code %d)", ctr.ExitCode))
		}
		_, status, last := b.get(ctx, port, creds, "max_results=1")
		if last == nil && (status == http.StatusOK || (creds.user == "" && status == http.StatusUnauthorized)) {
			return nil
		}
		if last == nil {
			last = fmt.Errorf("HTTP %d", status)
		}
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if !b.clk.Now().Before(deadline) {
			return b.startFailure(ctx, id, name, fmt.Sprintf("not ready within %s (NEBIUS_MOCKER_MSP_START_TIMEOUT): %v", b.cfg.StartTimeout, last))
		}
		if err := b.sleep(ctx, b.poll); err != nil {
			return err
		}
	}
}

func (b *Backend) startFailure(ctx context.Context, id, name, why string) error {
	logs, err := b.docker.Logs(context.WithoutCancel(ctx), id, logTail)
	if err != nil {
		logs = fmt.Sprintf("(no logs: %v)", err)
	}
	return errs.Internal(fmt.Sprintf("container %s %s; last log lines:\n%s", name, why, strings.TrimSpace(logs)))
}

// get calls the experiments search API and returns the body and status.
func (b *Backend) get(ctx context.Context, port string, creds credentials, query string) (body []byte, status int, err error) {
	u := url.URL{Scheme: "https", Host: net.JoinHostPort(b.adminHost(), port), Path: "/api/2.0/mlflow/experiments/search", RawQuery: query}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return nil, 0, err
	}
	if creds.user != "" {
		req.SetBasicAuth(creds.user, creds.password)
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err = io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return body, resp.StatusCode, err
}

// ExperimentsCount implements domain.Backend: the active experiments, paged.
func (b *Backend) ExperimentsCount(ctx context.Context, clusterID string) (int, error) {
	creds := b.credentialsOf(clusterID)
	ctr, err := b.docker.InspectContainer(ctx, b.containerName(clusterID))
	if err != nil {
		return 0, internal("inspect the tracking server", err)
	}
	if !ctr.Running {
		return 0, errs.BadState(clusterID, "the tracking server is not running")
	}
	if creds.user == "" {
		return 0, errs.Internal("the admin credentials of " + clusterID + " are unknown")
	}
	port, err := b.publishedPort(ctx, ctr.ID)
	if err != nil {
		return 0, err
	}
	count, token := 0, ""
	for {
		q := url.Values{"max_results": {"1000"}, "view_type": {"ACTIVE_ONLY"}}
		if token != "" {
			q.Set("page_token", token)
		}
		body, status, err := b.get(ctx, port, creds, q.Encode())
		if err != nil || status != http.StatusOK {
			return 0, errs.Internal(fmt.Sprintf("list experiments of %s: HTTP %d: %v", clusterID, status, err))
		}
		var page struct {
			Experiments   []json.RawMessage `json:"experiments"`
			NextPageToken string            `json:"next_page_token"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return 0, errs.Internal(fmt.Sprintf("decode experiments of %s: %v", clusterID, err))
		}
		count += len(page.Experiments)
		if page.NextPageToken == "" {
			return count, nil
		}
		token = page.NextPageToken
	}
}

// StopCluster implements domain.Backend.
func (b *Backend) StopCluster(ctx context.Context, clusterID string) error {
	cs, err := b.docker.ListContainers(ctx, b.clusterLabels(clusterID))
	if err != nil {
		return internal("list containers", err)
	}
	for _, c := range cs {
		if !c.Running {
			continue
		}
		if err := b.docker.StopContainer(ctx, c.ID, stopTimeout); err != nil && !errors.Is(err, dockerhost.ErrNotFound) {
			return internal("stop "+c.Name, err)
		}
	}
	return nil
}

// DeleteCluster implements domain.Backend.
func (b *Backend) DeleteCluster(ctx context.Context, clusterID string) error {
	b.adminsMu.Lock()
	delete(b.admins, clusterID)
	b.adminsMu.Unlock()
	return b.removeLabeled(ctx, b.clusterLabels(clusterID))
}
