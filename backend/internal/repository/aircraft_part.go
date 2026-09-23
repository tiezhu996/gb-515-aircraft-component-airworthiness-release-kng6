package repository

import (
	"context"

	"github.com/blueship581/aircraft-component-airworthiness-release/backend/internal/dto"
	"github.com/blueship581/aircraft-component-airworthiness-release/backend/internal/model"
	"gorm.io/gorm"
)

// AircraftPartRepository owns all persistence operations for 航空部件.
type AircraftPartRepository interface {
	List(context.Context, dto.PageQuery) (Page[model.AircraftPart], error)
	Get(context.Context, uint) (model.AircraftPart, error)
	GetByCode(context.Context, string) (model.AircraftPart, bool, error)
	Create(context.Context, *model.AircraftPart) error
	Update(context.Context, uint, uint, *model.AircraftPart) error
	// TransitionVersion moves the part with optimistic locking and, in the same
	// transaction when the part enters hold/retired, revokes every linked
	// approved/restricted authorization. The returned count lists revoked
	// authorization IDs so callers can report the linkage result.
	TransitionVersion(ctx context.Context, id, expectedVersion uint, item *model.AircraftPart, actor, requestID, before, reason string) ([]uint, error)
	Delete(context.Context, uint) error
	CountByStatus(context.Context) (map[string]int64, error)
}

type aircraftPartRepository struct {
	store *Store[model.AircraftPart]
	db    *gorm.DB
}

func NewAircraftPartRepository(db *gorm.DB) AircraftPartRepository {
	return &aircraftPartRepository{store: NewStore[model.AircraftPart](db), db: db}
}

func (r *aircraftPartRepository) List(ctx context.Context, q dto.PageQuery) (Page[model.AircraftPart], error) {
	return r.store.List(ctx, q)
}
func (r *aircraftPartRepository) Get(ctx context.Context, id uint) (model.AircraftPart, error) {
	return r.store.Get(ctx, id)
}
func (r *aircraftPartRepository) GetByCode(ctx context.Context, code string) (model.AircraftPart, bool, error) {
	var part model.AircraftPart
	err := r.db.WithContext(ctx).Where("code = ?", code).First(&part).Error
	if err == gorm.ErrRecordNotFound {
		return model.AircraftPart{}, false, nil
	}
	if err != nil {
		return model.AircraftPart{}, false, err
	}
	return part, true, nil
}
func (r *aircraftPartRepository) Create(ctx context.Context, item *model.AircraftPart) error {
	return r.store.Create(ctx, item)
}
func (r *aircraftPartRepository) Update(ctx context.Context, id, version uint, item *model.AircraftPart) error {
	return r.store.Update(ctx, id, version, item)
}

func (r *aircraftPartRepository) TransitionVersion(ctx context.Context, id, expectedVersion uint, item *model.AircraftPart, actor, requestID, before, reason string) ([]uint, error) {
	revokedIDs := make([]uint, 0)
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := optimisticUpdate(tx, id, expectedVersion, item); err != nil {
			return err
		}
		if err := appendAudit(tx, actor, requestID, "transition", "AircraftPart", id, before, item.Status, reason); err != nil {
			return err
		}
		// Part entered hold/retired: revoke linked active authorizations in
		// the same transaction. A concurrent part transition loses the version
		// predicate above, so this linkage runs at most once.
		if PartBlocksRelease(item.Status) {
			ids, err := RevokeAuthorizationsForPart(ctx, tx, model.AircraftPart{
				BaseModel: model.BaseModel{Code: item.Code, Status: item.Status},
			}, actor, requestID)
			if err != nil {
				return err
			}
			revokedIDs = ids
		}
		return nil
	})
	return revokedIDs, err
}
func (r *aircraftPartRepository) Delete(ctx context.Context, id uint) error {
	return r.store.Delete(ctx, id)
}
func (r *aircraftPartRepository) CountByStatus(ctx context.Context) (map[string]int64, error) {
	return r.store.CountByStatus(ctx)
}
