package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/blueship581/aircraft-component-airworthiness-release/backend/internal/constants"
	"github.com/blueship581/aircraft-component-airworthiness-release/backend/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// LinkageOutcome reports how the linked 航空部件 status affected a release
// authorization decision. Blocked decisions never mutate the authorization
// state machine; the persisted checkpoint is returned for read-back instead.
type LinkageOutcome struct {
	Blocked     bool
	PartCode    string
	PartStatus  string
	BlockReason string
}

// ErrLinkageBlocked signals a submit/approve decision rejected because the
// linked part is hold, retired or missing. The checkpoint columns have already
// been committed independently so the page can display the reason.
var ErrLinkageBlocked = fmt.Errorf("linked part blocks release decision")

// partStatusLabels keeps backend audit/error messages human readable without
// importing frontend assets.
var partStatusLabels = map[string]string{
	"received":   "已接收",
	"inspection": "检查中",
	"hold":       "暂停",
	"released":   "已放行",
	"retired":    "已退役",
}

// PartBlocksRelease reports whether a part state forbids submitting or
// approving a release authorization.
func PartBlocksRelease(status string) bool {
	return status == string(constants.PartStateHold) || status == string(constants.PartStateRetired)
}

// lockPartForUpdate reads the part referenced by an authorization relatedCode.
// MySQL takes a row lock so concurrent approve/part-transition requests
// serialize on the part; SQLite (local dev/test) skips the locking clause and
// relies on its single-writer model plus the optimistic version predicate.
func lockPartForUpdate(tx *gorm.DB, code string) (model.AircraftPart, bool, error) {
	var part model.AircraftPart
	query := tx.Model(&model.AircraftPart{}).Where("code = ?", code)
	if tx.Dialector.Name() == "mysql" {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	err := query.First(&part).Error
	if err == gorm.ErrRecordNotFound {
		return model.AircraftPart{}, false, nil
	}
	if err != nil {
		return model.AircraftPart{}, false, err
	}
	return part, true, nil
}

func partBlockReason(code, status string) string {
	label := partStatusLabels[status]
	if label == "" {
		label = status
	}
	return fmt.Sprintf("关联部件 %s 当前为 %s（%s），禁止提交复核或批准", code, label, status)
}

func missingPartReason(code string) string {
	return fmt.Sprintf("关联部件 %s 不存在，禁止提交复核或批准", code)
}

// PersistLinkageCheckpoint writes only linkage metadata in its own autocommit.
// It deliberately does not touch status/version so a blocked authorization
// keeps its original state and revision chain, while remaining readable after
// a page refresh.
func PersistLinkageCheckpoint(ctx context.Context, db *gorm.DB, id uint, partStatus, reason, result string) error {
	return db.WithContext(ctx).Model(&model.ReleaseAuthorization{}).Where("id = ?", id).Updates(map[string]any{
		"related_part_status": partStatus,
		"block_reason":        reason,
		"linkage_result":      result,
		"updated_at":          time.Now().UTC(),
	}).Error
}

// EvaluateAuthorizationLinkage runs inside the authorization transition
// transaction: it locks and evaluates the linked part. When blocked it returns
// an outcome with the reason and the caller commits the checkpoint
// independently via PersistLinkageCheckpoint while rolling back the state
// change. When clear the caller applies the checkpoint columns together with
// the optimistic state update.
func EvaluateAuthorizationLinkage(tx *gorm.DB, relatedCode string) (LinkageOutcome, error) {
	outcome := LinkageOutcome{PartCode: relatedCode}
	if relatedCode == "" {
		return outcome, nil
	}
	part, found, err := lockPartForUpdate(tx, relatedCode)
	if err != nil {
		return outcome, err
	}
	if !found {
		outcome.Blocked = true
		outcome.BlockReason = missingPartReason(relatedCode)
		return outcome, nil
	}
	outcome.PartStatus = part.Status
	if PartBlocksRelease(part.Status) {
		outcome.Blocked = true
		outcome.BlockReason = partBlockReason(relatedCode, part.Status)
	}
	return outcome, nil
}

// RevokeAuthorizationsForPart revokes every approved/restricted authorization
// linked to a part that just entered hold or retired. Each revocation keeps
// the approved/restricted revision intact and appends a new version plus an
// audit entry. The version predicate guarantees concurrent requests can only
// revoke each authorization once; losers update zero rows and are skipped, so
// no half-update survives transaction rollback. It returns the IDs of the
// authorizations revoked by this transaction.
func RevokeAuthorizationsForPart(ctx context.Context, tx *gorm.DB, part model.AircraftPart, actor, requestID string) ([]uint, error) {
	var authorizations []model.ReleaseAuthorization
	query := tx.WithContext(ctx).Model(&model.ReleaseAuthorization{}).
		Where("related_code = ? AND status IN ?", part.Code, []string{
			string(constants.AuthorizationStateApproved),
			string(constants.AuthorizationStateRestricted),
		})
	if tx.Dialector.Name() == "mysql" {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	if err := query.Find(&authorizations).Error; err != nil {
		return nil, err
	}
	revokedIDs := make([]uint, 0, len(authorizations))
	for _, authorization := range authorizations {
		newVersion := authorization.Version + 1
		now := time.Now().UTC()
		reason := fmt.Sprintf("linked part %s entered %s; release authorization revoked", part.Code, part.Status)
		result := tx.Model(&model.ReleaseAuthorization{}).
			Where("id = ? AND version = ?", authorization.ID, authorization.Version).
			Updates(map[string]any{
				"status":              string(constants.AuthorizationStateRevoked),
				"version":             newVersion,
				"related_part_status": part.Status,
				"block_reason":        "",
				"linkage_result":      constants.LinkageResultRevoked,
				"updated_at":          now,
			})
		if result.Error != nil {
			return revokedIDs, result.Error
		}
		if result.RowsAffected == 0 {
			// A concurrent request already moved this authorization; skipping
			// preserves the single-success guarantee.
			continue
		}
		revision := model.ReleaseAuthorizationRevision{
			ReleaseAuthorizationID: authorization.ID, Version: newVersion,
			Status: string(constants.AuthorizationStateRevoked), Evidence: authorization.Evidence,
			Actor: actor, RequestID: requestID, Action: "transition", Reason: reason, CreatedAt: now,
		}
		if err := tx.Create(&revision).Error; err != nil {
			return revokedIDs, err
		}
		if err := appendAudit(tx, actor, requestID, "transition", "ReleaseAuthorization", authorization.ID,
			authorization.Status, string(constants.AuthorizationStateRevoked), reason); err != nil {
			return revokedIDs, err
		}
		revokedIDs = append(revokedIDs, authorization.ID)
	}
	return revokedIDs, nil
}
