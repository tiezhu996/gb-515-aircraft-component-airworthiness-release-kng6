package repository

import (
	"context"
	"time"

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
	RecordLinkBlock(context.Context, uint, string, string, string, string) error
	RevokeLinkedActive(*gorm.DB, string, string, string, string) error
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

// RecordLinkBlock persists the reason a submit/approve attempt was blocked by
// the linked part state. The authorization version and status stay untouched
// (保持原状态); only the reason column and an audit entry are written.
func (r *releaseAuthorizationRepository) RecordLinkBlock(ctx context.Context, id uint, status, reason, actor, requestID string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&model.ReleaseAuthorization{}).Where("id = ?", id).
			UpdateColumns(map[string]any{"link_block_reason": reason})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return appendAudit(tx, actor, requestID, "link_blocked", "ReleaseAuthorization", id, status, status, reason)
	})
}

// RevokeLinkedActive runs inside the caller's transaction: every approved or
// restricted authorization linked to partCode is marked revoked with its own
// optimistic-lock check, a new revision (the approved version stays in the
// chain) and an audit entry. Any conflict aborts the whole transaction.
func (r *releaseAuthorizationRepository) RevokeLinkedActive(tx *gorm.DB, partCode, actor, requestID, detail string) error {
	active := []string{string(constants.AuthorizationStateApproved), string(constants.AuthorizationStateRestricted)}
	var items []model.ReleaseAuthorization
	if err := tx.Where("related_code = ? AND status IN ?", partCode, active).Find(&items).Error; err != nil {
		return err
	}
	for index := range items {
		item := items[index]
		before := item.Status
		expectedVersion := item.Version
		item.Status = string(constants.AuthorizationStateRevoked)
		item.Version = expectedVersion + 1
		item.UpdatedAt = time.Now().UTC()
		item.LinkBlockReason = ""
		item.LinkOutcome = detail
		item.Revisions = nil
		if err := optimisticUpdate(tx, item.ID, expectedVersion, &item); err != nil {
			return err
		}
		revision := model.ReleaseAuthorizationRevision{
			ReleaseAuthorizationID: item.ID, Version: item.Version, Status: item.Status,
			Evidence: item.Evidence, Actor: actor, RequestID: requestID, Action: "transition",
			Reason: detail, CreatedAt: item.UpdatedAt,
		}
		if err := tx.Create(&revision).Error; err != nil {
			return err
		}
		if err := appendAudit(tx, actor, requestID, "transition", "ReleaseAuthorization", item.ID, before, item.Status, detail); err != nil {
			return err
		}
	}
	return nil
}
func (r *releaseAuthorizationRepository) Delete(ctx context.Context, id uint) error {
	return r.store.Delete(ctx, id)
}
func (r *releaseAuthorizationRepository) CountByStatus(ctx context.Context) (map[string]int64, error) {
	return r.store.CountByStatus(ctx)
}
