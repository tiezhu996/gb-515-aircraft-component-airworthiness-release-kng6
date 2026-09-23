package service

import "errors"

var (
	ErrInvalidTransition = errors.New("requested status transition is not allowed")
	ErrInvalidInput      = errors.New("business input validation failed")
	ErrUnauthorized      = errors.New("invalid username or password")
	ErrInactiveUser      = errors.New("user account is inactive")
	ErrForbidden         = errors.New("role is not permitted for this operation")
	ErrLocked            = errors.New("record is locked after review begins")
	ErrSeparationOfDuty  = errors.New("preparer and reviewer must be different users")
)

// LinkageBlockedError is returned when a submit/approve decision is rejected
// because the linked part is hold, retired or missing. The authorization
// status and version are untouched; the block checkpoint is already persisted.
type LinkageBlockedError struct {
	PartCode   string
	PartStatus string
	Reason     string
}

func (e *LinkageBlockedError) Error() string {
	if e.Reason != "" {
		return e.Reason
	}
	return "linked part blocks release decision"
}
