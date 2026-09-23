package repository

import (
	"context"

	"github.com/blueship581/aircraft-component-airworthiness-release/backend/internal/constants"
	"github.com/blueship581/aircraft-component-airworthiness-release/backend/internal/dto"
	"github.com/blueship581/aircraft-component-airworthiness-release/backend/internal/model"
	"gorm.io/gorm"
)

// ReleaseAuthorizationRepository owns all persistence operations for 放行授权.
type ReleaseAuthorizationRepository interface {
	List(context.Context, dto.PageQuery) (Page[model.ReleaseAuthorization], error)
	Get(context.Context, uint) (model.ReleaseAuthorization, error)
	CreateVersion(context.Context, *model.ReleaseAuthorization, string, string) error
	UpdateVersion(context.Context, uint, uint, *model.ReleaseAuthorization, string, string, string, string, string) error
	// UpdateVersionWithLinkage behaves like UpdateVersion but enforces the
	// linked-part gate inside the same transaction before any state change.
	UpdateVersionWithLinkage(context.Context, uint, uint, *model.ReleaseAuthorization, string, string, string, string, string) (LinkageOutcome, error)
	Delete(context.Context, uint) error
	CountByStatus(context.Context) (map[string]int64, error)
}

type releaseAuthorizationRepository struct {
	store *Store[model.ReleaseAuthorization]
	db    *gorm.DB
}

func NewReleaseAuthorizationRepository(db *gorm.DB) ReleaseAuthorizationRepository {
	return &releaseAuthorizationRepository{store: NewStore[model.ReleaseAuthorization](db), db: db}
}

func (r *releaseAuthorizationRepository) List(ctx context.Context, q dto.PageQuery) (Page[model.ReleaseAuthorization], error) {
	page, err := r.store.List(ctx, q)
	if err != nil || len(page.Items) == 0 {
		return page, err
	}
	ids := make([]uint, 0, len(page.Items))
	for _, item := range page.Items {
		ids = append(ids, item.ID)
	}
	var revisions []model.ReleaseAuthorizationRevision
	if err := r.db.WithContext(ctx).Where("release_authorization_id IN ?", ids).
		Order("release_authorization_id, version").Find(&revisions).Error; err != nil {
		return Page[model.ReleaseAuthorization]{}, err
	}
	byAuthorization := make(map[uint][]model.ReleaseAuthorizationRevision)
	for _, revision := range revisions {
		byAuthorization[revision.ReleaseAuthorizationID] = append(byAuthorization[revision.ReleaseAuthorizationID], revision)
	}
	for index := range page.Items {
		page.Items[index].Revisions = byAuthorization[page.Items[index].ID]
	}
	return page, nil
}
func (r *releaseAuthorizationRepository) Get(ctx context.Context, id uint) (model.ReleaseAuthorization, error) {
	var item model.ReleaseAuthorization
	err := r.db.WithContext(ctx).Preload("Revisions", func(db *gorm.DB) *gorm.DB {
		return db.Order("version")
	}).First(&item, id).Error
	return item, err
}

func (r *releaseAuthorizationRepository) CreateVersion(ctx context.Context, item *model.ReleaseAuthorization, actor, requestID string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Omit("Revisions").Create(item).Error; err != nil {
			return err
		}
		revision := model.ReleaseAuthorizationRevision{
			ReleaseAuthorizationID: item.ID, Version: item.Version, Status: item.Status,
			Evidence: item.Evidence, Actor: actor, RequestID: requestID, Action: "create",
			Reason: "authorization drafted", CreatedAt: item.CreatedAt,
		}
		if err := tx.Create(&revision).Error; err != nil {
			return err
		}
		return appendAudit(tx, actor, requestID, "create", "ReleaseAuthorization", item.ID, "", item.Status, "authorization version 1 drafted")
	})
}

func (r *releaseAuthorizationRepository) UpdateVersion(ctx context.Context, id, expectedVersion uint, item *model.ReleaseAuthorization, actor, requestID, action, before, reason string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		item.Revisions = nil
		if err := optimisticUpdate(tx, id, expectedVersion, item); err != nil {
			return err
		}
		revision := model.ReleaseAuthorizationRevision{
			ReleaseAuthorizationID: id, Version: item.Version, Status: item.Status,
			Evidence: item.Evidence, Actor: actor, RequestID: requestID, Action: action,
			Reason: reason, CreatedAt: item.UpdatedAt,
		}
		if err := tx.Create(&revision).Error; err != nil {
			return err
		}
		return appendAudit(tx, actor, requestID, action, "ReleaseAuthorization", id, before, item.Status, reason)
	})
}

func (r *releaseAuthorizationRepository) UpdateVersionWithLinkage(ctx context.Context, id, expectedVersion uint, item *model.ReleaseAuthorization, actor, requestID, action, before, reason string) (LinkageOutcome, error) {
	outcome := LinkageOutcome{PartCode: item.RelatedCode}
	if item.RelatedCode == "" {
		// No linked part: run the normal versioned transition and clear any
		// stale linkage checkpoint in the same atomic write.
		item.RelatedPartStatus = ""
		item.BlockReason = ""
		item.LinkageResult = constants.LinkageResultUnlinked
		return outcome, r.UpdateVersion(ctx, id, expectedVersion, item, actor, requestID, action, before, reason)
	}

	// Begin explicitly so a blocked decision can rollback while its checkpoint
	// is committed independently. The part row lock is held until commit,
	// preventing a part transition from racing the approval.
	tx := r.db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return outcome, tx.Error
	}
	checkpoint, err := EvaluateAuthorizationLinkage(tx, item.RelatedCode)
	if err != nil {
		_ = tx.Rollback().Error
		return outcome, err
	}
	outcome = checkpoint
	if checkpoint.Blocked {
		if rbErr := tx.Rollback().Error; rbErr != nil {
			return outcome, rbErr
		}
		// Commit only the linkage metadata (no status/version/revision change).
		if err := PersistLinkageCheckpoint(ctx, r.db, id, checkpoint.PartStatus, checkpoint.BlockReason, constants.LinkageResultBlocked); err != nil {
			return outcome, err
		}
		return outcome, ErrLinkageBlocked
	}

	item.RelatedPartStatus = checkpoint.PartStatus
	item.BlockReason = ""
	item.LinkageResult = constants.LinkageResultClear
	item.Revisions = nil
	if err := optimisticUpdate(tx, id, expectedVersion, item); err != nil {
		_ = tx.Rollback().Error
		return outcome, err
	}
	revision := model.ReleaseAuthorizationRevision{
		ReleaseAuthorizationID: id, Version: item.Version, Status: item.Status,
		Evidence: item.Evidence, Actor: actor, RequestID: requestID, Action: action,
		Reason: reason, CreatedAt: item.UpdatedAt,
	}
	if err := tx.Create(&revision).Error; err != nil {
		_ = tx.Rollback().Error
		return outcome, err
	}
	if err := appendAudit(tx, actor, requestID, action, "ReleaseAuthorization", id, before, item.Status, reason); err != nil {
		_ = tx.Rollback().Error
		return outcome, err
	}
	return outcome, tx.Commit().Error
}
func (r *releaseAuthorizationRepository) Delete(ctx context.Context, id uint) error {
	return r.store.Delete(ctx, id)
}
func (r *releaseAuthorizationRepository) CountByStatus(ctx context.Context) (map[string]int64, error) {
	return r.store.CountByStatus(ctx)
}
