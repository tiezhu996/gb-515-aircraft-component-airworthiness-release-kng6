package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blueship581/aircraft-component-airworthiness-release/backend/internal/dto"
	"github.com/blueship581/aircraft-component-airworthiness-release/backend/internal/model"
	"github.com/blueship581/aircraft-component-airworthiness-release/backend/internal/repository"
	"gorm.io/gorm"
)

// The release authorization gate and the part-driven revocation are verified
// against a real database so optimistic locking and transaction rollback run
// exactly as in production.

func TestAuthorizationSubmitBlockedWhenPartOnHold(t *testing.T) {
	db, partRepo, authRepo, _, authService := newLinkageStack(t)
	ctx := context.Background()
	part := seedLinkagePart(t, partRepo, "PART-HELD", "hold")
	auth := createLinkedAuthorization(t, authService, "AUTH-HELD", part.Code)

	_, err := authService.Transition(ctx, auth.ID, dto.TransitionRequest{
		Status: "review", ExpectedVersion: auth.Version, Reason: "submit for dual review",
	}, "operator", model.RoleOperator, "req-block-submit")
	var blocked *PartLinkBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("expected PartLinkBlockedError, got %v", err)
	}
	if blocked.PartCode != part.Code || blocked.PartStatus != "hold" {
		t.Fatalf("blocked error must carry the part 编号 and status: %#v", blocked)
	}

	persisted, err := authRepo.Get(ctx, auth.ID)
	if err != nil {
		t.Fatalf("reload authorization: %v", err)
	}
	if persisted.Status != "draft" || persisted.Version != 1 {
		t.Fatalf("blocked submit must keep original status and version: %#v", persisted.BaseModel)
	}
	if !strings.Contains(persisted.LinkBlockReason, part.Code) {
		t.Fatalf("block reason must be persisted with the part code: %q", persisted.LinkBlockReason)
	}

	view, err := authService.Get(ctx, auth.ID)
	if err != nil {
		t.Fatalf("re-read authorization: %v", err)
	}
	if view.LinkedPartStatus != "hold" || !strings.Contains(view.LinkBlockReason, part.Code) {
		t.Fatalf("release page re-read must show part status and block reason: %#v", view)
	}
	assertActionAudits(t, db, auth.ID, "link_blocked", 1)
}

func TestAuthorizationApproveBlockedWhenPartRetired(t *testing.T) {
	db, partRepo, authRepo, partService, authService := newLinkageStack(t)
	ctx := context.Background()
	part := seedLinkagePart(t, partRepo, "PART-RET", "inspection")
	auth := createLinkedAuthorization(t, authService, "AUTH-RET", part.Code)

	auth = transitionAuthorization(t, authService, auth, "review", "operator", model.RoleOperator, "req-ret-review")
	part = transitionPart(t, partService, part, "hold", "req-ret-hold")
	part = transitionPart(t, partService, part, "retired", "req-ret-retire")

	reviewState, err := authRepo.Get(ctx, auth.ID)
	if err != nil {
		t.Fatalf("reload authorization: %v", err)
	}
	if reviewState.Status != "review" {
		t.Fatalf("review-state authorization must not be revoked by the linkage, got %s", reviewState.Status)
	}

	_, err = authService.Transition(ctx, auth.ID, dto.TransitionRequest{
		Status: "approved", ExpectedVersion: auth.Version, Reason: "independent release review passed",
	}, "reviewer", model.RoleReviewer, "req-ret-approve")
	var blocked *PartLinkBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("expected PartLinkBlockedError, got %v", err)
	}
	if blocked.PartCode != part.Code || blocked.PartStatus != "retired" {
		t.Fatalf("blocked approval must return the part 编号 and status: %#v", blocked)
	}
	persisted, err := authRepo.Get(ctx, auth.ID)
	if err != nil {
		t.Fatalf("reload authorization: %v", err)
	}
	if persisted.Status != "review" || persisted.Version != auth.Version {
		t.Fatalf("blocked approval must keep original status: %#v", persisted.BaseModel)
	}
	assertActionAudits(t, db, auth.ID, "link_blocked", 1)
}

func TestAuthorizationGateKeepsOriginalPermissionOrder(t *testing.T) {
	_, partRepo, _, partService, authService := newLinkageStack(t)
	ctx := context.Background()
	part := seedLinkagePart(t, partRepo, "PART-PERM", "inspection")
	auth := createLinkedAuthorization(t, authService, "AUTH-PERM", part.Code)
	auth = transitionAuthorization(t, authService, auth, "review", "operator", model.RoleOperator, "req-perm-review")
	transitionPart(t, partService, part, "hold", "req-perm-hold")

	approval := dto.TransitionRequest{Status: "approved", ExpectedVersion: auth.Version, Reason: "permission order check"}
	if _, err := authService.Transition(ctx, auth.ID, approval, "operator", model.RoleOperator, "req-perm-operator"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("operator approval must stay forbidden, got %v", err)
	}
	if _, err := authService.Transition(ctx, auth.ID, approval, "operator", model.RoleReviewer, "req-perm-same-user"); !errors.Is(err, ErrSeparationOfDuty) {
		t.Fatalf("same-user approval must stay rejected, got %v", err)
	}
	var blocked *PartLinkBlockedError
	if _, err := authService.Transition(ctx, auth.ID, approval, "reviewer", model.RoleReviewer, "req-perm-blocked"); !errors.As(err, &blocked) {
		t.Fatalf("reviewer approval must hit the part gate, got %v", err)
	}
}

func TestAuthorizationGateClearsAfterPartRecovers(t *testing.T) {
	_, partRepo, authRepo, partService, authService := newLinkageStack(t)
	ctx := context.Background()
	part := seedLinkagePart(t, partRepo, "PART-RECOVER", "hold")
	auth := createLinkedAuthorization(t, authService, "AUTH-RECOVER", part.Code)

	if _, err := authService.Transition(ctx, auth.ID, dto.TransitionRequest{
		Status: "review", ExpectedVersion: auth.Version, Reason: "first attempt",
	}, "operator", model.RoleOperator, "req-recover-blocked"); err == nil {
		t.Fatal("submit must be blocked while the part is on hold")
	}

	part = transitionPart(t, partService, part, "inspection", "req-recover-inspection")
	auth = transitionAuthorization(t, authService, auth, "review", "operator", model.RoleOperator, "req-recover-review")

	persisted, err := authRepo.Get(ctx, auth.ID)
	if err != nil {
		t.Fatalf("reload authorization: %v", err)
	}
	if persisted.LinkBlockReason != "" {
		t.Fatalf("successful submit must clear the persisted block reason, got %q", persisted.LinkBlockReason)
	}
	view, err := authService.Get(ctx, auth.ID)
	if err != nil {
		t.Fatalf("re-read authorization: %v", err)
	}
	if view.LinkedPartStatus != "inspection" || view.Status != "review" {
		t.Fatalf("release page must show the recovered part status: %#v", view)
	}
}

func TestAuthorizationWithoutLinkedPartProceeds(t *testing.T) {
	_, _, _, _, authService := newLinkageStack(t)
	auth := createLinkedAuthorization(t, authService, "AUTH-NOPART", "PART-MISSING")
	auth = transitionAuthorization(t, authService, auth, "review", "operator", model.RoleOperator, "req-nopart-review")
	if auth.Status != "review" {
		t.Fatalf("authorization without a linked part must continue dual review, got %s", auth.Status)
	}
}

func TestPartHoldRevokesApprovedAuthorizationsAtomically(t *testing.T) {
	db, partRepo, authRepo, partService, authService := newLinkageStack(t)
	ctx := context.Background()
	part := seedLinkagePart(t, partRepo, "PART-LINK", "released")

	authA := createLinkedAuthorization(t, authService, "AUTH-LINK-A", part.Code)
	authA = transitionAuthorization(t, authService, authA, "review", "operator", model.RoleOperator, "link-a-review")
	authA = transitionAuthorization(t, authService, authA, "approved", "reviewer", model.RoleReviewer, "link-a-approve")

	authB := createLinkedAuthorization(t, authService, "AUTH-LINK-B", part.Code)
	authB = transitionAuthorization(t, authService, authB, "review", "operator", model.RoleOperator, "link-b-review")
	authB = transitionAuthorization(t, authService, authB, "approved", "reviewer", model.RoleReviewer, "link-b-approve")
	authB = transitionAuthorization(t, authService, authB, "restricted", "reviewer", model.RoleReviewer, "link-b-restrict")

	updated, err := partService.Transition(ctx, part.ID, dto.TransitionRequest{
		Status: "hold", ExpectedVersion: part.Version, Reason: "quality escape requires hold",
	}, "operator", "link-part-hold")
	if err != nil {
		t.Fatalf("transition part to hold: %v", err)
	}
	if updated.Status != "hold" {
		t.Fatalf("part must reach hold, got %s", updated.Status)
	}

	revokedA, err := authRepo.Get(ctx, authA.ID)
	if err != nil {
		t.Fatalf("reload authorization A: %v", err)
	}
	if revokedA.Status != "revoked" || revokedA.Version != authA.Version+1 {
		t.Fatalf("approved authorization must be revoked by the linkage: %#v", revokedA.BaseModel)
	}
	if !strings.Contains(revokedA.LinkOutcome, part.Code) {
		t.Fatalf("linkage outcome must reference the part code: %q", revokedA.LinkOutcome)
	}
	if len(revokedA.Revisions) != 4 {
		t.Fatalf("revocation must append a revision, got %d", len(revokedA.Revisions))
	}
	if approved := revokedA.Revisions[2]; approved.Status != "approved" {
		t.Fatalf("批准版本必须保留, got revision %#v", approved)
	}
	if latest := revokedA.Revisions[3]; latest.Status != "revoked" || !strings.Contains(latest.Reason, part.Code) {
		t.Fatalf("revocation revision must explain the linkage: %#v", latest)
	}

	revokedB, err := authRepo.Get(ctx, authB.ID)
	if err != nil {
		t.Fatalf("reload authorization B: %v", err)
	}
	if revokedB.Status != "revoked" || revokedB.Version != authB.Version+1 {
		t.Fatalf("restricted authorization must be revoked by the linkage: %#v", revokedB.BaseModel)
	}

	assertAuditChain(t, db, "ReleaseAuthorization", authA.ID, 4)
	assertAuditChain(t, db, "ReleaseAuthorization", authB.ID, 5)
	assertAuditChain(t, db, "AircraftPart", part.ID, 1)
}

func TestConcurrentPartTransitionRevokesAuthorizationOnce(t *testing.T) {
	db, partRepo, authRepo, partService, authService := newLinkageStack(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("unwrap sql db: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	ctx := context.Background()
	part := seedLinkagePart(t, partRepo, "PART-RACE", "released")
	auth := createLinkedAuthorization(t, authService, "AUTH-RACE", part.Code)
	auth = transitionAuthorization(t, authService, auth, "review", "operator", model.RoleOperator, "race-review")
	auth = transitionAuthorization(t, authService, auth, "approved", "reviewer", model.RoleReviewer, "race-approve")

	const attempts = 8
	results := make(chan error, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := partService.Transition(ctx, part.ID, dto.TransitionRequest{
				Status: "hold", ExpectedVersion: part.Version, Reason: "concurrent hold",
			}, fmt.Sprintf("operator-%d", i), fmt.Sprintf("race-hold-%d", i))
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)

	successes := 0
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		// The loser either reads the stale version and hits the optimistic lock,
		// or observes the committed hold state and is rejected by the state graph.
		// Both failures prove the loser applied nothing.
		if !errors.Is(err, repository.ErrVersionConflict) && !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("loser must fail without side effects, got %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("并发下只能成功一次, got %d successes", successes)
	}

	persistedPart, err := partRepo.Get(ctx, part.ID)
	if err != nil {
		t.Fatalf("reload part: %v", err)
	}
	if persistedPart.Status != "hold" || persistedPart.Version != part.Version+1 {
		t.Fatalf("part must be transitioned exactly once: %#v", persistedPart.BaseModel)
	}
	revoked, err := authRepo.Get(ctx, auth.ID)
	if err != nil {
		t.Fatalf("reload authorization: %v", err)
	}
	if revoked.Status != "revoked" || revoked.Version != auth.Version+1 || len(revoked.Revisions) != 4 {
		t.Fatalf("authorization must be revoked exactly once: %#v", revoked.BaseModel)
	}
	assertAuditChain(t, db, "ReleaseAuthorization", auth.ID, 4)
	assertAuditChain(t, db, "AircraftPart", part.ID, 1)
}

func TestPartLinkageFailureLeavesNoPartialUpdate(t *testing.T) {
	db, partRepo, authRepo, partService, authService := newLinkageStack(t)
	ctx := context.Background()
	part := seedLinkagePart(t, partRepo, "PART-JAM", "released")
	auth := createLinkedAuthorization(t, authService, "AUTH-JAM", part.Code)
	auth = transitionAuthorization(t, authService, auth, "review", "operator", model.RoleOperator, "jam-review")
	auth = transitionAuthorization(t, authService, auth, "approved", "reviewer", model.RoleReviewer, "jam-approve")

	// Jam the revision chain so the linked revocation fails mid-transaction.
	jammed := model.ReleaseAuthorizationRevision{
		ReleaseAuthorizationID: auth.ID, Version: auth.Version + 1, Status: "approved",
		Actor: "jam", RequestID: "jam", Action: "seed", CreatedAt: time.Now().UTC(),
	}
	if err := db.Create(&jammed).Error; err != nil {
		t.Fatalf("jam revision chain: %v", err)
	}

	if _, err := partService.Transition(ctx, part.ID, dto.TransitionRequest{
		Status: "hold", ExpectedVersion: part.Version, Reason: "hold must roll back",
	}, "operator", "jam-hold"); err == nil {
		t.Fatal("linkage failure must abort the part transition")
	}

	persistedPart, err := partRepo.Get(ctx, part.ID)
	if err != nil {
		t.Fatalf("reload part: %v", err)
	}
	if persistedPart.Status != "released" || persistedPart.Version != part.Version {
		t.Fatalf("failed linkage must roll the part transition back: %#v", persistedPart.BaseModel)
	}
	persistedAuth, err := authRepo.Get(ctx, auth.ID)
	if err != nil {
		t.Fatalf("reload authorization: %v", err)
	}
	if persistedAuth.Status != "approved" || persistedAuth.Version != auth.Version {
		t.Fatalf("failed linkage must not touch the authorization: %#v", persistedAuth.BaseModel)
	}
	assertAuditChain(t, db, "AircraftPart", part.ID, 0)
	assertAuditChain(t, db, "ReleaseAuthorization", auth.ID, 3)
}

func TestAuthorizationListCarriesLinkedPartStatus(t *testing.T) {
	_, partRepo, _, _, authService := newLinkageStack(t)
	ctx := context.Background()
	part := seedLinkagePart(t, partRepo, "PART-LIST", "inspection")
	createLinkedAuthorization(t, authService, "AUTH-LIST", part.Code)

	page, err := authService.List(ctx, dto.PageQuery{Page: 1, PageSize: 20})
	if err != nil {
		t.Fatalf("list authorizations: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("expected one authorization, got %d", len(page.Items))
	}
	if page.Items[0].LinkedPartStatus != "inspection" {
		t.Fatalf("list must carry the live linked part status, got %q", page.Items[0].LinkedPartStatus)
	}
}

func newLinkageStack(t *testing.T) (*gorm.DB, repository.AircraftPartRepository, repository.ReleaseAuthorizationRepository, AircraftPartService, ReleaseAuthorizationService) {
	t.Helper()
	db := newVersionTestDB(t)
	partRepo := repository.NewAircraftPartRepository(db)
	authRepo := repository.NewReleaseAuthorizationRepository(db)
	partService := NewAircraftPartService(partRepo, nil, authRepo)
	authService := NewReleaseAuthorizationService(authRepo, nil, partRepo)
	return db, partRepo, authRepo, partService, authService
}

func seedLinkagePart(t *testing.T, repo repository.AircraftPartRepository, code, status string) model.AircraftPart {
	t.Helper()
	part := model.AircraftPart{
		BaseModel: model.BaseModel{
			Code: code, Name: "Linkage part " + code, Status: status, Version: 1,
			Description: "part linkage fixture",
		},
		Facility: "Linkage hangar", Owner: "Linkage crew", Category: "engine",
		RiskLevel: "medium", MetricValue: 10, MetricUnit: "percent",
		EffectiveAt: time.Now().UTC(), Evidence: "linkage evidence",
	}
	if err := repo.Create(context.Background(), &part); err != nil {
		t.Fatalf("seed part %s: %v", code, err)
	}
	return part
}

func createLinkedAuthorization(t *testing.T, service ReleaseAuthorizationService, code, relatedCode string) model.ReleaseAuthorization {
	t.Helper()
	input := authorizationInput(code)
	input.RelatedCode = relatedCode
	created, err := service.Create(context.Background(), input, "operator", code+"-create")
	if err != nil {
		t.Fatalf("create authorization %s: %v", code, err)
	}
	return created
}

func transitionAuthorization(t *testing.T, service ReleaseAuthorizationService, auth model.ReleaseAuthorization, target, actor, role, requestID string) model.ReleaseAuthorization {
	t.Helper()
	updated, err := service.Transition(context.Background(), auth.ID, dto.TransitionRequest{
		Status: target, ExpectedVersion: auth.Version, Reason: "linkage test transition to " + target,
	}, actor, role, requestID)
	if err != nil {
		t.Fatalf("transition authorization %s to %s: %v", auth.Code, target, err)
	}
	return updated
}

func transitionPart(t *testing.T, service AircraftPartService, part model.AircraftPart, target, requestID string) model.AircraftPart {
	t.Helper()
	updated, err := service.Transition(context.Background(), part.ID, dto.TransitionRequest{
		Status: target, ExpectedVersion: part.Version, Reason: "linkage test transition to " + target,
	}, "operator", requestID)
	if err != nil {
		t.Fatalf("transition part %s to %s: %v", part.Code, target, err)
	}
	return updated
}

func assertActionAudits(t *testing.T, db *gorm.DB, entityID uint, action string, expected int64) {
	t.Helper()
	var count int64
	if err := db.Model(&model.AuditLog{}).
		Where("entity_id = ? AND action = ?", entityID, action).Count(&count).Error; err != nil {
		t.Fatalf("count %s audits: %v", action, err)
	}
	if count != expected {
		t.Fatalf("expected %d %s audits, got %d", expected, action, count)
	}
}
