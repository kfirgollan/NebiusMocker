//go:build e2e

// Package mk8s runs the official nebius CLI against NebiusMocker through the
// Managed Kubernetes flows the Nebius docs show
// (https://docs.nebius.com/kubernetes/clusters/manage,
// https://docs.nebius.com/kubernetes/node-groups/manage,
// https://docs.nebius.com/kubernetes/connect) and the Applications release
// commands, with the default mock backend: clusters, node groups and their
// node instances are managed, and get-credentials writes the kubeconfig real
// Nebius writes, though nothing serves the Kubernetes API.
package mk8s

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/applications"
	"github.com/kfirgollan/NebiusMocker/services/compute"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/mk8s"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
	"github.com/kfirgollan/NebiusMocker/test/internal/nebiuscli"
)

type metadata struct {
	ID       string            `json:"id"`
	ParentID string            `json:"parent_id"`
	Name     string            `json:"name"`
	Labels   map[string]string `json:"labels"`
}

type cluster struct {
	Metadata metadata `json:"metadata"`
	Spec     struct {
		ControlPlane struct {
			Version  string `json:"version"`
			SubnetID string `json:"subnet_id"`
		} `json:"control_plane"`
	} `json:"spec"`
	Status struct {
		State        string `json:"state"`
		ControlPlane struct {
			Version   string `json:"version"`
			Endpoints struct {
				Public  string `json:"public_endpoint"`
				Private string `json:"private_endpoint"`
			} `json:"endpoints"`
			Auth struct {
				CA string `json:"cluster_ca_certificate"`
			} `json:"auth"`
		} `json:"control_plane"`
	} `json:"status"`
}

type nodeGroup struct {
	Metadata metadata `json:"metadata"`
	Spec     struct {
		FixedNodeCount string `json:"fixed_node_count"`
	} `json:"spec"`
	Status struct {
		State          string `json:"state"`
		Version        string `json:"version"`
		NodeCount      string `json:"node_count"`
		ReadyNodeCount string `json:"ready_node_count"`
	} `json:"status"`
}

type instance struct {
	Metadata metadata `json:"metadata"`
	Status   struct {
		State             string `json:"state"`
		NetworkInterfaces []struct {
			IPAddress struct {
				Address string `json:"address"`
			} `json:"ip_address"`
			PublicIPAddress struct {
				Address string `json:"address"`
			} `json:"public_ip_address"`
		} `json:"network_interfaces"`
	} `json:"status"`
}

type release struct {
	Metadata metadata `json:"metadata"`
	Spec     struct {
		ClusterID       string `json:"cluster_id"`
		ProductSlug     string `json:"product_slug"`
		Namespace       string `json:"namespace"`
		ApplicationName string `json:"application_name"`
	} `json:"spec"`
	Status struct {
		State string `json:"state"`
	} `json:"status"`
}

// kubeconfig is the part of a kubeconfig file the test checks.
type kubeconfig struct {
	CurrentContext string `yaml:"current-context"`
	Clusters       []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server string `yaml:"server"`
			CAData string `yaml:"certificate-authority-data"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster string `yaml:"cluster"`
			User    string `yaml:"user"`
		} `yaml:"context"`
	} `yaml:"contexts"`
	Users []struct {
		Name string `yaml:"name"`
		User struct {
			Exec struct {
				APIVersion string   `yaml:"apiVersion"`
				Command    string   `yaml:"command"`
				Args       []string `yaml:"args"`
			} `yaml:"exec"`
		} `yaml:"user"`
	} `yaml:"users"`
}

type env struct {
	inst    *testkit.Instance
	cli     *nebiuscli.CLI
	viewer  *nebiuscli.CLI
	project string
}

func start(t *testing.T) *env {
	t.Helper()
	inst := testkit.StartT(t, []mocker.Module{iam.New(), vpc.New(), compute.New(), mk8s.New(), applications.New()},
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce), testkit.WithOperationDelay(100*time.Millisecond))
	p := nebiuscli.Profile{Addr: inst.Addr, Token: inst.Token, ParentID: inst.ProjectID, TenantID: inst.TenantID}
	cli := nebiuscli.New(t, p)
	p.Token = config.DefaultViewerToken
	return &env{inst: inst, cli: cli, viewer: nebiuscli.New(t, p), project: inst.ProjectID}
}

func (e *env) fails(t *testing.T, c *nebiuscli.CLI, want string, args ...string) {
	t.Helper()
	_, err := c.Run(t.Context(), args...)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("nebius %s: err = %v, want one mentioning %q", strings.Join(args, " "), err, want)
	}
}

func (e *env) defaultSubnet(t *testing.T) string {
	t.Helper()
	var sn struct {
		Metadata metadata `json:"metadata"`
	}
	e.cli.JSON(t, &sn, "vpc", "subnet", "get-by-name", "--name", "default-subnet")
	return sn.Metadata.ID
}

// nodes lists the project's compute instances that belong to the node group.
func nodes(t *testing.T, c *nebiuscli.CLI, nodeGroupID string) []instance {
	t.Helper()
	var list struct {
		Items []instance `json:"items"`
	}
	c.JSON(t, &list, "compute", "instance", "list")
	return slices.DeleteFunc(list.Items, func(i instance) bool {
		return i.Metadata.Labels["mk8s.nebius.ai/node-group-id"] != nodeGroupID
	})
}

// TestClusterNodeGroupAndCredentials follows the docs: create a cluster and
// a node group, inspect the node instances, get credentials, scale, upgrade,
// install an application release and clean up. A viewer reads everything.
func TestClusterNodeGroupAndCredentials(t *testing.T) {
	e := start(t)
	subnet := e.defaultSubnet(t)

	var versions struct {
		Items []struct {
			Version string `json:"version"`
		} `json:"items"`
	}
	e.cli.JSON(t, &versions, "mk8s", "cluster", "list-control-plane-versions")
	if len(versions.Items) == 0 {
		t.Fatal("list-control-plane-versions returned nothing")
	}

	// nebius mk8s cluster create --name … --control-plane-subnet-id … --control-plane-endpoints-public-endpoint=true
	var c cluster
	e.cli.JSON(t, &c, "mk8s", "cluster", "create", "--name", "demo", "--control-plane-version", "1.35",
		"--control-plane-subnet-id", subnet, "--control-plane-endpoints-public-endpoint=true")
	if !strings.HasPrefix(c.Metadata.ID, "mk8scluster-") || c.Status.State != "RUNNING" ||
		!strings.HasPrefix(c.Status.ControlPlane.Version, "1.35.") || !strings.HasPrefix(c.Status.ControlPlane.Endpoints.Public, "https://") {
		t.Fatalf("cluster create = %+v", c)
	}
	var byName cluster
	e.viewer.JSON(t, &byName, "mk8s", "cluster", "get-by-name", "--name", "demo")
	if byName.Metadata.ID != c.Metadata.ID || byName.Status.ControlPlane.Auth.CA == "" {
		t.Fatalf("viewer get-by-name = %+v", byName)
	}

	e.checkCredentials(t, c)

	// nebius mk8s node-group create --parent-id … --fixed-node-count 2 --template-resources-platform … --template-resources-preset …
	var ng nodeGroup
	e.cli.JSON(t, &ng, "mk8s", "node-group", "create", "--parent-id", c.Metadata.ID, "--name", "cpu",
		"--fixed-node-count", "2", "--template-resources-platform", "cpu-d3", "--template-resources-preset", "2vcpu-8gb",
		"--template-boot-disk-size-gibibytes", "64", "--template-boot-disk-type", "network_ssd",
		"--template-network-interfaces", `[{"public_ip_address": {}}]`, "--auto-approve")
	if ng.Status.State != "RUNNING" || ng.Status.NodeCount != "2" || ng.Status.ReadyNodeCount != "2" {
		t.Fatalf("node-group create = %+v", ng)
	}
	ns := nodes(t, e.viewer, ng.Metadata.ID)
	if len(ns) != 2 {
		t.Fatalf("viewer sees %d node instances, want 2", len(ns))
	}
	for _, n := range ns {
		nic := n.Status.NetworkInterfaces
		if n.Status.State != "RUNNING" || len(nic) != 1 || nic[0].IPAddress.Address == "" || nic[0].PublicIPAddress.Address == "" {
			t.Fatalf("node instance = %+v", n)
		}
	}
	// Node instances belong to the node group.
	e.fails(t, e.cli, "FailedPrecondition", "compute", "instance", "delete", "--id", ns[0].Metadata.ID)
	e.fails(t, e.cli, "FailedPrecondition", "compute", "instance", "stop", "--id", ns[0].Metadata.ID)
	e.fails(t, e.viewer, "PermissionDenied", "mk8s", "node-group", "delete", "--id", ng.Metadata.ID)

	var matrix struct {
		Versions []struct {
			KubernetesVersion string `json:"kubernetes_version"`
		} `json:"versions"`
	}
	e.cli.JSON(t, &matrix, "mk8s", "node-group", "get-compatibility-matrix", "--cluster-kubernetes-version", "1.35", "--platform", "cpu-d3")
	if len(matrix.Versions) == 0 {
		t.Fatal("get-compatibility-matrix returned no versions")
	}

	// Scale down, then upgrade to the latest node image.
	e.cli.JSON(t, &ng, "mk8s", "node-group", "update", "--id", ng.Metadata.ID, "--fixed-node-count", "1", "--auto-approve")
	if got := nodes(t, e.cli, ng.Metadata.ID); ng.Status.NodeCount != "1" || len(got) != 1 {
		t.Fatalf("after scale-down: node group %+v, %d instances", ng.Status, len(got))
	}
	e.cli.JSON(t, &ng, "mk8s", "node-group", "upgrade", "--id", ng.Metadata.ID, "--latest-infra-version")
	if !strings.Contains(ng.Status.Version, "-nebius-node.") {
		t.Fatalf("after upgrade: version %q", ng.Status.Version)
	}

	e.checkRelease(t, c.Metadata.ID)

	var groups struct {
		Items []nodeGroup `json:"items"`
	}
	e.viewer.JSON(t, &groups, "mk8s", "node-group", "list", "--parent-id", c.Metadata.ID)
	if len(groups.Items) != 1 {
		t.Fatalf("viewer node-group list = %+v", groups)
	}

	e.cli.JSON(t, &json.RawMessage{}, "mk8s", "node-group", "delete", "--id", ng.Metadata.ID)
	if got := nodes(t, e.cli, ng.Metadata.ID); len(got) != 0 {
		t.Fatalf("%d node instances left after node-group delete", len(got))
	}
	e.cli.JSON(t, &json.RawMessage{}, "mk8s", "cluster", "delete", "--id", c.Metadata.ID)
	e.fails(t, e.cli, "NotFound", "mk8s", "cluster", "get", "--id", c.Metadata.ID)
}

// checkCredentials runs get-credentials and get-token as the docs show and
// checks the kubeconfig: the public endpoint and CA of the cluster, and an
// exec credential plugin that calls the CLI for an IAM token.
func (e *env) checkCredentials(t *testing.T, c cluster) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if _, err := e.cli.Run(t.Context(), "mk8s", "cluster", "get-credentials", "--id", c.Metadata.ID, "--external", "--kubeconfig", path); err != nil {
		t.Fatalf("get-credentials: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var kc kubeconfig
	if err := yaml.Unmarshal(data, &kc); err != nil {
		t.Fatalf("kubeconfig: %v\n%s", err, data)
	}
	want := "nebius-mk8s-demo-" + strings.TrimPrefix(c.Metadata.ID, "mk8scluster-")
	if len(kc.Clusters) != 1 || len(kc.Users) != 1 || len(kc.Contexts) != 1 || kc.CurrentContext != want {
		t.Fatalf("kubeconfig:\n%s", data)
	}
	cl, u := kc.Clusters[0], kc.Users[0]
	ca, err := base64.StdEncoding.DecodeString(cl.Cluster.CAData)
	if cl.Name != want || cl.Cluster.Server != c.Status.ControlPlane.Endpoints.Public || err != nil || string(ca) != c.Status.ControlPlane.Auth.CA {
		t.Fatalf("kubeconfig cluster = %+v (CA decode err %v)", cl, err)
	}
	ex := u.User.Exec
	if ex.APIVersion != "client.authentication.k8s.io/v1beta1" || !filepath.IsAbs(ex.Command) || filepath.Base(ex.Command) != "nebius" ||
		!slices.Equal(ex.Args[:4], []string{"mk8s", "v1", "cluster", "get-token"}) {
		t.Fatalf("kubeconfig user exec = %+v", ex)
	}

	// The exec plugin's own command, as kubectl would run it.
	var cred struct {
		Kind   string `json:"kind"`
		Status struct {
			Token string `json:"token"`
		} `json:"status"`
	}
	e.cli.JSON(t, &cred, ex.Args...)
	if cred.Kind != "ExecCredential" || cred.Status.Token != e.inst.Token {
		t.Fatalf("get-token = %+v", cred)
	}
}

// checkRelease installs an application release into the cluster and reads
// it back as a viewer.
func (e *env) checkRelease(t *testing.T, clusterID string) {
	t.Helper()
	var r release
	e.cli.JSON(t, &r, "applications", "v1alpha1", "k8s-release", "create", "--cluster-id", clusterID,
		"--product-slug", "nebius/soperator", "--namespace", "soperator", "--application-name", "soperator")
	if !strings.HasPrefix(r.Metadata.ID, "applicationrelease-") || r.Status.State != "DEPLOYED" {
		t.Fatalf("k8s-release create = %+v", r)
	}
	var list struct {
		Items []release `json:"items"`
	}
	e.viewer.JSON(t, &list, "applications", "v1alpha1", "k8s-release", "list", "--cluster-id", clusterID)
	if len(list.Items) != 1 || list.Items[0].Spec.ProductSlug != "nebius/soperator" || list.Items[0].Spec.Namespace != "soperator" {
		t.Fatalf("viewer k8s-release list = %+v", list)
	}
	e.cli.JSON(t, &json.RawMessage{}, "applications", "v1alpha1", "k8s-release", "delete", "--id", r.Metadata.ID)
}
