package compute

import (
	"fmt"

	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
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
