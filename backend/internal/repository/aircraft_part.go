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
	FindByCodes(context.Context, []string) ([]model.AircraftPart, error)
	Create(context.Context, *model.AircraftPart) error
	Update(context.Context, uint, uint, *model.AircraftPart) error
	TransitionWithLinkage(context.Context, uint, uint, *model.AircraftPart, string, string, string, string, func(*gorm.DB) error) error
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

// FindByCodes loads parts by their human-facing codes so release authorizations
// can resolve the linked part status without knowing internal identifiers.
func (r *aircraftPartRepository) FindByCodes(ctx context.Context, codes []string) ([]model.AircraftPart, error) {
	items := make([]model.AircraftPart, 0, len(codes))
	if len(codes) == 0 {
		return items, nil
	}
	err := r.db.WithContext(ctx).Where("code IN ?", codes).Find(&items).Error
	return items, err
}

func (r *aircraftPartRepository) Create(ctx context.Context, item *model.AircraftPart) error {
	return r.store.Create(ctx, item)
}
func (r *aircraftPartRepository) Update(ctx context.Context, id, version uint, item *model.AircraftPart) error {
	return r.store.Update(ctx, id, version, item)
}

// TransitionWithLinkage applies the optimistic part update, its audit entry and
// the authorization linkage callback inside a single transaction. A concurrent
// transition loses the optimistic-lock race and any linkage failure rolls the
// whole transition back, so a failed request never leaves a partial update.
func (r *aircraftPartRepository) TransitionWithLinkage(ctx context.Context, id, expectedVersion uint, item *model.AircraftPart, actor, requestID, before, reason string, linkage func(*gorm.DB) error) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := optimisticUpdate(tx, id, expectedVersion, item); err != nil {
			return err
		}
		if err := appendAudit(tx, actor, requestID, "transition", "AircraftPart", id, before, item.Status, reason); err != nil {
			return err
		}
		if linkage != nil {
			return linkage(tx)
		}
		return nil
	})
}
func (r *aircraftPartRepository) Delete(ctx context.Context, id uint) error {
	return r.store.Delete(ctx, id)
}
func (r *aircraftPartRepository) CountByStatus(ctx context.Context) (map[string]int64, error) {
	return r.store.CountByStatus(ctx)
}
