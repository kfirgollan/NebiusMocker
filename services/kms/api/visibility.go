package api

import "github.com/kfirgollan/NebiusMocker/services/kms/domain"

// scheduledGetter is implemented by Get and List requests.
type scheduledGetter interface{ GetShowScheduledForDeletion() bool }

// visibility maps a request's show_scheduled_for_deletion flag.
func visibility(req scheduledGetter) domain.Visibility {
	if req.GetShowScheduledForDeletion() {
		return domain.IncludeScheduled
	}
	return domain.ActiveOnly
}
