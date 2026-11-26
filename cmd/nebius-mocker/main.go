// Command nebius-mocker runs the all-in-one NebiusMocker server.
//
// Configuration comes from NEBIUS_MOCKER_* environment variables; see
// docs/architecture/11-configuration.md.
package main

import (
	"os"

	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/services/ai"
	"github.com/kfirgollan/NebiusMocker/services/applications"
	"github.com/kfirgollan/NebiusMocker/services/audit"
	"github.com/kfirgollan/NebiusMocker/services/billing"
	"github.com/kfirgollan/NebiusMocker/services/capacity"
	"github.com/kfirgollan/NebiusMocker/services/compute"
	"github.com/kfirgollan/NebiusMocker/services/dns"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/kms"
	"github.com/kfirgollan/NebiusMocker/services/mk8s"
	"github.com/kfirgollan/NebiusMocker/services/mysterybox"
	"github.com/kfirgollan/NebiusMocker/services/quotas"
	"github.com/kfirgollan/NebiusMocker/services/registry"
	"github.com/kfirgollan/NebiusMocker/services/storage"
	"github.com/kfirgollan/NebiusMocker/services/tunnel"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
)

func main() {
	os.Exit(mocker.Main(iam.New(), compute.New(), registry.New(), kms.New(), storage.New(), mysterybox.New(), vpc.New(), ai.New(), audit.New(), dns.New(), mk8s.New(), applications.New(), tunnel.New(), capacity.New(), billing.New(), quotas.New()))
}
