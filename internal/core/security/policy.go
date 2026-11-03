package security

import (
	"videoarchive/internal/core/approval"
	"videoarchive/internal/core/confirmation"
	"videoarchive/internal/core/vocabulary"
)

type UserRole = vocabulary.UserRole

const (
	Operator = vocabulary.RoleOperator
	Admin    = vocabulary.RoleAdmin
)

type Action string

const (
	Read           Action = "read"
	Process        Action = "process"
	Commit         Action = "commit"
	Destructive    Action = "destructive"
	ManageSecurity Action = "manage_security"
)

type Request struct {
	ActorType          vocabulary.ActorType
	Role               UserRole
	Action             Action
	TargetID           string
	TargetRevision     uint64
	AuthMode           AuthMode
	ArchivistMCPDirect bool
	ConfigPolicyAllows bool
	Approvals          []approval.Record
	Confirmation       *confirmation.Record
}

func Authorize(request Request) bool {
	switch request.Action {
	case Read:
		return request.ActorType.Valid()
	case Process:
		if request.ActorType == vocabulary.ActorArchivist {
			return request.ArchivistMCPDirect
		}
		return request.ActorType == vocabulary.ActorHuman && request.Role.Valid()
	case Commit:
		return request.ActorType == vocabulary.ActorHuman && request.Role.Valid() &&
			approval.Approved(request.Approvals, request.TargetID, request.TargetRevision, "")
	case Destructive:
		confirmed := request.ActorType == vocabulary.ActorHuman && request.Confirmation != nil &&
			request.Confirmation.ValidFor(request.TargetID)
		approved := approval.Approved(request.Approvals, request.TargetID, request.TargetRevision, "")
		if request.AuthMode == AuthDisabled {
			return request.Role == Operator && request.ConfigPolicyAllows && approved && confirmed
		}
		return request.AuthMode == AuthLocal && request.Role == Admin && approved && confirmed
	case ManageSecurity:
		return false
	default:
		return false
	}
}
