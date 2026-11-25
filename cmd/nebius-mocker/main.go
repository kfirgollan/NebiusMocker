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
)

func main() {
	os.Exit(mocker.Main(iam.New(), compute.New()))
}
