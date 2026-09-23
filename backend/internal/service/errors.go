package service

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidTransition = errors.New("requested status transition is not allowed")
	ErrInvalidInput      = errors.New("business input validation failed")
	ErrUnauthorized      = errors.New("invalid username or password")
	ErrInactiveUser      = errors.New("user account is inactive")
	ErrForbidden         = errors.New("role is not permitted for this operation")
	ErrLocked            = errors.New("record is locked after review begins")
	ErrSeparationOfDuty  = errors.New("preparer and reviewer must be different users")
)

// PartLinkBlockedError is returned when a release authorization submit/approve
// is blocked because the linked aircraft part is on hold or retired. The
// authorization keeps its current status and the part code travels with the
// error so callers can surface the blocking 编号.
type PartLinkBlockedError struct {
	PartCode   string
	PartStatus string
	Action     string
}

func (e *PartLinkBlockedError) Error() string {
	return fmt.Sprintf("关联部件 %s 当前状态 %s，%s被阻止，授权保持原状态", e.PartCode, e.PartStatus, e.Action)
}
