// Command nebius-mocker runs the all-in-one NebiusMocker server.
//
// Configuration comes from NEBIUS_MOCKER_* environment variables; see
// docs/architecture/11-configuration.md.
package main

import (
	"os"

	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/services/compute"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/kms"
	"github.com/kfirgollan/NebiusMocker/services/registry"
)

func main() {
	os.Exit(mocker.Main(iam.New(), compute.New(), registry.New(), kms.New()))
}
