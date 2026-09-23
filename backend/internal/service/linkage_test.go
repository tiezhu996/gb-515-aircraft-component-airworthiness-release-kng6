package service

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/blueship581/aircraft-component-airworthiness-release/backend/internal/dto"
	"github.com/blueship581/aircraft-component-airworthiness-release/backend/internal/model"
	"github.com/blueship581/aircraft-component-airworthiness-release/backend/internal/repository"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func newLinkageTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "linkage.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&model.AuditLog{}, &model.AircraftPart{},
		&model.ReleaseAuthorization{}, &model.ReleaseAuthorizationRevision{},
	); err != nil {
		t.Fatalf("migrate sqlite: %v", err)
	}
	return db
}

func submitAndApprove(t *testing.T, ctx context.Context, db *gorm.DB, partCode string) (releaseService ReleaseAuthorizationService, authorization model.ReleaseAuthorization) {
	t.Helper()
	releaseService = NewReleaseAuthorizationService(repository.NewReleaseAuthorizationRepository(db), nil)
	created, err := releaseService.Create(ctx, authorizationInputFor(partCode, "RA-LINK-"+partCode), "operator", "create")
	if err != nil {
		t.Fatalf("create authorization: %v", err)
	}
	if _, err := releaseService.Transition(ctx, created.ID, dto.TransitionRequest{
		Status: "review", ExpectedVersion: created.Version, Reason: "evidence ready for review",
	}, "operator", model.RoleOperator, "submit"); err != nil {
		t.Fatalf("submit authorization: %v", err)
	}
	authorization, err = releaseService.Transition(ctx, created.ID, dto.TransitionRequest{
		Status: "approved", ExpectedVersion: 2, Reason: "independent release review passed",
	}, "reviewer", model.RoleReviewer, "approve")
	if err != nil {
		t.Fatalf("approve authorization: %v", err)
	}
	return releaseService, authorization
}

func authorizationInputFor(partCode, authCode string) dto.CreateReleaseAuthorization {
	input := authorizationInput(authCode)
	input.RelatedCode = partCode
	return input
}

func TestSubmitBlockedWhenPartHoldKeepsStateAndPersistsCheckpoint(t *testing.T) {
	db := newLinkageTestDB(t)
	seedPart(t, db, "PART-HOLD", "hold")
	service := NewReleaseAuthorizationService(repository.NewReleaseAuthorizationRepository(db), nil)
	ctx := context.Background()

	created, err := service.Create(ctx, authorizationInputFor("PART-HOLD", "RA-BLOCK-1"), "operator", "create")
	if err != nil {
		t.Fatalf("create authorization: %v", err)
	}
	_, err = service.Transition(ctx, created.ID, dto.TransitionRequest{
		Status: "review", ExpectedVersion: created.Version, Reason: "attempt submit against held part",
	}, "operator", model.RoleOperator, "submit-blocked")

	var blocked *LinkageBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("expected LinkageBlockedError, got %v", err)
	}
	if blocked.PartCode != "PART-HOLD" {
		t.Fatalf("blocked response must carry related code, got %q", blocked.PartCode)
	}

	// State and version must be untouched and readable back with the reason.
	after, err := service.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("reload authorization: %v", err)
	}
	if after.Status != "draft" || after.Version != 1 {
		t.Fatalf("blocked authorization must keep draft v1, got %s v%d", after.Status, after.Version)
	}
	if after.RelatedPartStatus != "hold" || after.LinkageResult != "blocked" || after.BlockReason == "" {
		t.Fatalf("block checkpoint not persisted: %#v", after)
	}
	if len(after.Revisions) != 1 {
		t.Fatalf("blocked submit must not append a revision, got %d", len(after.Revisions))
	}
	assertAuditChain(t, db, "ReleaseAuthorization", created.ID, 1)
}

func TestSubmitBlockedWhenPartMissing(t *testing.T) {
	db := newLinkageTestDB(t)
	service := NewReleaseAuthorizationService(repository.NewReleaseAuthorizationRepository(db), nil)
	ctx := context.Background()

	created, err := service.Create(ctx, authorizationInputFor("PART-GHOST", "RA-MISSING-1"), "operator", "create")
	if err != nil {
		t.Fatalf("create authorization: %v", err)
	}
	_, err = service.Transition(ctx, created.ID, dto.TransitionRequest{
		Status: "review", ExpectedVersion: created.Version, Reason: "attempt submit with dangling link",
	}, "operator", model.RoleOperator, "submit-missing")
	var blocked *LinkageBlockedError
	if !errors.As(err, &blocked) || blocked.PartCode != "PART-GHOST" {
		t.Fatalf("missing part must block and return the code, got %v", err)
	}
	after, _ := service.Get(ctx, created.ID)
	if after.Status != "draft" || after.Version != 1 || after.LinkageResult != "blocked" {
		t.Fatalf("dangling link must preserve draft v1, got %#v", after)
	}
}

func TestApproveBlockedWhenPartRetired(t *testing.T) {
	db := newLinkageTestDB(t)
	// A releasable part allows submit; retiring it before approval blocks.
	seedPart(t, db, "PART-RETIRE", "released")
	releaseService := NewReleaseAuthorizationService(repository.NewReleaseAuthorizationRepository(db), nil)
	partService := NewAircraftPartService(repository.NewAircraftPartRepository(db), nil)
	ctx := context.Background()

	created, err := releaseService.Create(ctx, authorizationInputFor("PART-RETIRE", "RA-RETIRE-1"), "operator", "create")
	if err != nil {
		t.Fatalf("create authorization: %v", err)
	}
	review, err := releaseService.Transition(ctx, created.ID, dto.TransitionRequest{
		Status: "review", ExpectedVersion: 1, Reason: "evidence ready",
	}, "operator", model.RoleOperator, "submit")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	part, found, err := repository.NewAircraftPartRepository(db).GetByCode(ctx, "PART-RETIRE")
	if err != nil || !found {
		t.Fatalf("load part: %v found=%v", err, found)
	}
	if _, err := partService.Transition(ctx, part.ID, dto.TransitionRequest{
		Status: "retired", ExpectedVersion: part.Version, Reason: "retired before approval",
	}, "operator", "part-retire"); err != nil {
		t.Fatalf("retire part: %v", err)
	}

	_, err = releaseService.Transition(ctx, review.ID, dto.TransitionRequest{
		Status: "approved", ExpectedVersion: review.Version, Reason: "approve against retired part",
	}, "reviewer", model.RoleReviewer, "approve-blocked")
	var blocked *LinkageBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("approval against retired part must block, got %v", err)
	}
	after, _ := releaseService.Get(ctx, review.ID)
	if after.Status != "review" {
		t.Fatalf("blocked approval must keep review state, got %s", after.Status)
	}
	if after.RelatedPartStatus != "retired" || after.BlockReason == "" {
		t.Fatalf("retired checkpoint missing: %#v", after)
	}
}

func TestPartHoldAtomicallyRevokesApprovedAuthorization(t *testing.T) {
	db := newLinkageTestDB(t)
	seedPart(t, db, "PART-REL", "released")
	ctx := context.Background()
	partRepo := repository.NewAircraftPartRepository(db)
	part, found, err := partRepo.GetByCode(ctx, "PART-REL")
	if err != nil || !found {
		t.Fatalf("load part: %v found=%v", err, found)
	}
	releaseService, authorization := submitAndApprove(t, ctx, db, "PART-REL")
	approvedVersion := authorization.Version

	partService := NewAircraftPartService(partRepo, nil)
	updated, err := partService.Transition(ctx, part.ID, dto.TransitionRequest{
		Status: "hold", ExpectedVersion: part.Version, Reason: "quality freeze",
	}, "operator", "part-hold")
	if err != nil {
		t.Fatalf("move part to hold: %v", err)
	}
	if updated.Status != "hold" {
		t.Fatalf("part status = %s", updated.Status)
	}

	revoked, err := releaseService.Get(ctx, authorization.ID)
	if err != nil {
		t.Fatalf("reload authorization: %v", err)
	}
	if revoked.Status != "revoked" || revoked.Version != approvedVersion+1 || revoked.LinkageResult != "revoked" {
		t.Fatalf("authorization must be revoked atomically: %#v", revoked)
	}
	// The approved revision is preserved, followed by the revocation revision.
	if len(revoked.Revisions) != int(approvedVersion)+1 {
		t.Fatalf("approved version must be retained, got %d revisions", len(revoked.Revisions))
	}
	if prior := revoked.Revisions[approvedVersion-1]; prior.Status != "approved" {
		t.Fatalf("prior approved revision lost: %#v", prior)
	}
	if latest := revoked.Revisions[len(revoked.Revisions)-1]; latest.Status != "revoked" || latest.RequestID != "part-hold" {
		t.Fatalf("revocation revision mismatch: %#v", latest)
	}
	assertAuditChain(t, db, "ReleaseAuthorization", authorization.ID, int64(approvedVersion+1))

	var partAudits, revokeAudits int64
	_ = db.Model(&model.AuditLog{}).Where("entity_type = ? AND entity_id = ? AND after_state = ?", "AircraftPart", part.ID, "hold").Count(&partAudits).Error
	_ = db.Model(&model.AuditLog{}).Where("entity_type = ? AND entity_id = ? AND after_state = ?", "ReleaseAuthorization", authorization.ID, "revoked").Count(&revokeAudits).Error
	if partAudits != 1 || revokeAudits != 1 {
		t.Fatalf("expected one part audit and one revoke audit, got %d/%d", partAudits, revokeAudits)
	}
}

func TestConcurrentPartTransitionsRevokeOnlyOnce(t *testing.T) {
	db := newLinkageTestDB(t)
	seedPart(t, db, "PART-CONC", "released")
	ctx := context.Background()
	part, found, err := repository.NewAircraftPartRepository(db).GetByCode(ctx, "PART-CONC")
	if err != nil || !found {
		t.Fatalf("load part: %v found=%v", err, found)
	}
	_, authorization := submitAndApprove(t, ctx, db, "PART-CONC")

	partService := NewAircraftPartService(repository.NewAircraftPartRepository(db), nil)
	start := make(chan struct{})
	var wg sync.WaitGroup
	successes := make(chan bool, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := partService.Transition(ctx, part.ID, dto.TransitionRequest{
				Status: "hold", ExpectedVersion: part.Version, Reason: "concurrent freeze",
			}, "operator", "part-hold-concurrent")
			successes <- err == nil
		}()
	}
	close(start)
	wg.Wait()
	close(successes)
	ok := 0
	for s := range successes {
		if s {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("exactly one concurrent part transition must succeed, got %d", ok)
	}

	releaseRepo := repository.NewReleaseAuthorizationRepository(db)
	reloaded, err := releaseRepo.Get(ctx, authorization.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Status != "revoked" {
		t.Fatalf("authorization must be revoked, got %s", reloaded.Status)
	}
	if reloaded.Version != authorization.Version+1 {
		t.Fatalf("authorization may only gain one revocation version, got v%d", reloaded.Version)
	}
	assertAuditChain(t, db, "ReleaseAuthorization", authorization.ID, int64(authorization.Version+1))
}

func TestDraftEditClearsStaleBlockCheckpoint(t *testing.T) {
	db := newLinkageTestDB(t)
	seedPart(t, db, "PART-DRAFT", "hold")
	service := NewReleaseAuthorizationService(repository.NewReleaseAuthorizationRepository(db), nil)
	ctx := context.Background()

	created, err := service.Create(ctx, authorizationInputFor("PART-DRAFT", "RA-DRAFT-1"), "operator", "create")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := service.Transition(ctx, created.ID, dto.TransitionRequest{
		Status: "review", ExpectedVersion: 1, Reason: "blocked first attempt",
	}, "operator", model.RoleOperator, "blocked"); err == nil {
		t.Fatal("expected block against held part")
	}
	// Edit the draft to clear the linkage, then submit proceeds without a part.
	updated, err := service.Update(ctx, created.ID, dto.UpdateReleaseAuthorization{
		ExpectedVersion: 1, Name: created.Name, Description: created.Description, Facility: created.Facility,
		Owner: created.Owner, Category: created.Category, RiskLevel: created.RiskLevel, MetricValue: created.MetricValue,
		MetricUnit: created.MetricUnit, EffectiveAt: created.EffectiveAt, Evidence: created.Evidence, RelatedCode: "",
	}, "operator", "edit-clear")
	if err != nil {
		t.Fatalf("draft edit: %v", err)
	}
	if updated.BlockReason != "" || updated.LinkageResult != "" || updated.RelatedPartStatus != "" {
		t.Fatalf("draft edit must clear checkpoint: %#v", updated)
	}
	if _, err := service.Transition(ctx, created.ID, dto.TransitionRequest{
		Status: "review", ExpectedVersion: 2, Reason: "resubmit without linkage",
	}, "operator", model.RoleOperator, "resubmit"); err != nil {
		t.Fatalf("unlinked authorization should submit, got %v", err)
	}
}

func TestRestrictedAuthorizationAlsoRevokedOnPartRetire(t *testing.T) {
	db := newLinkageTestDB(t)
	seedPart(t, db, "PART-RES", "released")
	ctx := context.Background()
	partRepo := repository.NewAircraftPartRepository(db)
	part, found, _ := partRepo.GetByCode(ctx, "PART-RES")
	if !found {
		t.Fatal("part missing")
	}
	releaseService := NewReleaseAuthorizationService(repository.NewReleaseAuthorizationRepository(db), nil)
	created, err := releaseService.Create(ctx, authorizationInputFor("PART-RES", "RA-RES-1"), "operator", "create")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := releaseService.Transition(ctx, created.ID, dto.TransitionRequest{
		Status: "review", ExpectedVersion: 1, Reason: "ready",
	}, "operator", model.RoleOperator, "submit"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	restricted, err := releaseService.Transition(ctx, created.ID, dto.TransitionRequest{
		Status: "restricted", ExpectedVersion: 2, Reason: "limited release conditions",
	}, "reviewer", model.RoleReviewer, "restrict")
	if err != nil {
		t.Fatalf("restrict: %v", err)
	}
	partService := NewAircraftPartService(partRepo, nil)
	if _, err := partService.Transition(ctx, part.ID, dto.TransitionRequest{
		Status: "retired", ExpectedVersion: part.Version, Reason: "end of life",
	}, "operator", "part-retire"); err != nil {
		t.Fatalf("retire part: %v", err)
	}
	reloaded, err := releaseService.Get(ctx, restricted.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Status != "revoked" || reloaded.Version != 4 || reloaded.LinkageResult != "revoked" {
		t.Fatalf("restricted authorization must be revoked, got %#v", reloaded)
	}
}

func TestPartRetireRevokesMultipleLinkedAuthorizationsAtomically(t *testing.T) {
	db := newLinkageTestDB(t)
	seedPart(t, db, "PART-MULTI", "released")
	ctx := context.Background()
	part, found, err := repository.NewAircraftPartRepository(db).GetByCode(ctx, "PART-MULTI")
	if err != nil || !found {
		t.Fatalf("load part: %v found=%v", err, found)
	}
	releaseService := NewReleaseAuthorizationService(repository.NewReleaseAuthorizationRepository(db), nil)
	ids := make([]uint, 0, 2)
	for _, code := range []string{"RA-MULTI-1", "RA-MULTI-2"} {
		created, err := releaseService.Create(ctx, authorizationInputFor("PART-MULTI", code), "operator", "create")
		if err != nil {
			t.Fatalf("create %s: %v", code, err)
		}
		if _, err := releaseService.Transition(ctx, created.ID, dto.TransitionRequest{
			Status: "review", ExpectedVersion: 1, Reason: "ready",
		}, "operator", model.RoleOperator, "submit"); err != nil {
			t.Fatalf("submit %s: %v", code, err)
		}
		approved, err := releaseService.Transition(ctx, created.ID, dto.TransitionRequest{
			Status: "approved", ExpectedVersion: 2, Reason: "independent review passed",
		}, "reviewer", model.RoleReviewer, "approve")
		if err != nil {
			t.Fatalf("approve %s: %v", code, err)
		}
		ids = append(ids, approved.ID)
	}
	partService := NewAircraftPartService(repository.NewAircraftPartRepository(db), nil)
	if _, err := partService.Transition(ctx, part.ID, dto.TransitionRequest{
		Status: "retired", ExpectedVersion: part.Version, Reason: "fleet retirement",
	}, "operator", "part-retire-multi"); err != nil {
		t.Fatalf("retire part: %v", err)
	}
	for _, id := range ids {
		reloaded, err := releaseService.Get(ctx, id)
		if err != nil {
			t.Fatalf("reload %d: %v", id, err)
		}
		if reloaded.Status != "revoked" || reloaded.Version != 4 || reloaded.LinkageResult != "revoked" {
			t.Fatalf("authorization %d must be revoked v4, got %s v%d", id, reloaded.Status, reloaded.Version)
		}
	}
}
