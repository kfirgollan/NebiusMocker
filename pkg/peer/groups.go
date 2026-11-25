package peer

import "context"

// GroupMembersKey is the capability provided by the iam module: group
// membership, for resource policies that grant access to a group (Object
// Storage bucket policies).
var GroupMembersKey = NewKey[GroupMembers]("iam.group-members")

// GroupMembers answers group membership questions.
type GroupMembers interface {
	// IsMember reports whether principalID currently belongs to groupID
	// (memberships past their revoke_at do not count).
	IsMember(ctx context.Context, principalID, groupID string) (bool, error)
}
