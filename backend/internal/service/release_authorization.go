package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/blueship581/aircraft-component-airworthiness-release/backend/internal/constants"
	"github.com/blueship581/aircraft-component-airworthiness-release/backend/internal/dto"
	"github.com/blueship581/aircraft-component-airworthiness-release/backend/internal/model"
	"github.com/blueship581/aircraft-component-airworthiness-release/backend/internal/repository"
)

type ReleaseAuthorizationService interface {
	List(context.Context, dto.PageQuery) (repository.Page[model.ReleaseAuthorization], error)
	Get(context.Context, uint) (model.ReleaseAuthorization, error)
	Create(context.Context, dto.CreateReleaseAuthorization, string, string) (model.ReleaseAuthorization, error)
	Update(context.Context, uint, dto.UpdateReleaseAuthorization, string, string) (model.ReleaseAuthorization, error)
	Transition(context.Context, uint, dto.TransitionRequest, string, string, string) (model.ReleaseAuthorization, error)
	Delete(context.Context, uint, string, string) error
	StatusCounts(context.Context) (map[string]int64, error)
}

type releaseAuthorizationService struct {
	repository repository.ReleaseAuthorizationRepository
	security   SecurityService
	parts      repository.AircraftPartRepository
}

func NewReleaseAuthorizationService(repo repository.ReleaseAuthorizationRepository, security SecurityService, parts repository.AircraftPartRepository) ReleaseAuthorizationService {
	return &releaseAuthorizationService{repository: repo, security: security, parts: parts}
}

func (s *releaseAuthorizationService) List(ctx context.Context, query dto.PageQuery) (repository.Page[model.ReleaseAuthorization], error) {
	page, err := s.repository.List(ctx, query)
	if err != nil {
		return page, err
	}
	if err := s.attachLinkedPartStatus(ctx, page.Items); err != nil {
		return repository.Page[model.ReleaseAuthorization]{}, err
	}
	return page, nil
}

func (s *releaseAuthorizationService) Get(ctx context.Context, id uint) (model.ReleaseAuthorization, error) {
	item, err := s.repository.Get(ctx, id)
	if err != nil {
		return item, err
	}
	items := []model.ReleaseAuthorization{item}
	if err := s.attachLinkedPartStatus(ctx, items); err != nil {
		return model.ReleaseAuthorization{}, err
	}
	return items[0], nil
}

// attachLinkedPartStatus resolves the live status of every linked part so the
// release page can re-read 部件状态 after any refresh.
func (s *releaseAuthorizationService) attachLinkedPartStatus(ctx context.Context, items []model.ReleaseAuthorization) error {
	codes := make([]string, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		if item.RelatedCode != "" && !seen[item.RelatedCode] {
			seen[item.RelatedCode] = true
			codes = append(codes, item.RelatedCode)
		}
	}
	if len(codes) == 0 {
		return nil
	}
	parts, err := s.parts.FindByCodes(ctx, codes)
	if err != nil {
		return err
	}
	statusByCode := make(map[string]string, len(parts))
	for _, part := range parts {
		statusByCode[part.Code] = part.Status
	}
	for index := range items {
		items[index].LinkedPartStatus = statusByCode[items[index].RelatedCode]
	}
	return nil
}

func (s *releaseAuthorizationService) Create(ctx context.Context, input dto.CreateReleaseAuthorization, actor, requestID string) (model.ReleaseAuthorization, error) {
	if err := validateReleaseAuthorizationBusinessFields(input.Code, input.Name, input.Facility, input.Owner); err != nil {
		return model.ReleaseAuthorization{}, err
	}
	item := model.ReleaseAuthorization{
		BaseModel: model.BaseModel{
			Code: strings.ToUpper(strings.TrimSpace(input.Code)), Name: strings.TrimSpace(input.Name),
			Status: model.ReleaseAuthorizationInitialStatus, Version: 1, Description: strings.TrimSpace(input.Description),
		},
		Facility: strings.TrimSpace(input.Facility), Owner: strings.TrimSpace(input.Owner),
		Category: strings.TrimSpace(input.Category), RiskLevel: input.RiskLevel,
		MetricValue: input.MetricValue, MetricUnit: strings.TrimSpace(input.MetricUnit),
		EffectiveAt: input.EffectiveAt.UTC(), Evidence: strings.TrimSpace(input.Evidence),
		RelatedCode: strings.ToUpper(strings.TrimSpace(input.RelatedCode)),
	}
	if err := s.repository.CreateVersion(ctx, &item, actor, requestID); err != nil {
		return model.ReleaseAuthorization{}, fmt.Errorf("create 放行授权: %w", err)
	}
	return s.Get(ctx, item.ID)
}

func (s *releaseAuthorizationService) Update(ctx context.Context, id uint, input dto.UpdateReleaseAuthorization, actor, requestID string) (model.ReleaseAuthorization, error) {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return model.ReleaseAuthorization{}, err
	}
	if current.Status != model.ReleaseAuthorizationInitialStatus {
		return model.ReleaseAuthorization{}, ErrLocked
	}
	if err := validateReleaseAuthorizationBusinessFields(current.Code, input.Name, input.Facility, input.Owner); err != nil {
		return model.ReleaseAuthorization{}, err
	}
	current.Name = strings.TrimSpace(input.Name)
	current.Description = strings.TrimSpace(input.Description)
	current.Facility = strings.TrimSpace(input.Facility)
	current.Owner = strings.TrimSpace(input.Owner)
	current.Category = strings.TrimSpace(input.Category)
	current.RiskLevel = input.RiskLevel
	current.MetricValue = input.MetricValue
	current.MetricUnit = strings.TrimSpace(input.MetricUnit)
	current.EffectiveAt = input.EffectiveAt.UTC()
	current.Evidence = strings.TrimSpace(input.Evidence)
	current.RelatedCode = strings.ToUpper(strings.TrimSpace(input.RelatedCode))
	current.Version = input.ExpectedVersion + 1
	current.UpdatedAt = time.Now().UTC()
	if err := s.repository.UpdateVersion(ctx, id, input.ExpectedVersion, &current, actor, requestID, "update", current.Status, "draft authorization fields updated"); err != nil {
		return model.ReleaseAuthorization{}, fmt.Errorf("update 放行授权: %w", err)
	}
	return s.Get(ctx, id)
}

func (s *releaseAuthorizationService) Transition(ctx context.Context, id uint, input dto.TransitionRequest, actor, role, requestID string) (model.ReleaseAuthorization, error) {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return model.ReleaseAuthorization{}, err
	}
	target := strings.TrimSpace(input.Status)
	if !constants.CanTransition(constants.ReleaseAuthorizationTransitions, current.Status, target) {
		return model.ReleaseAuthorization{}, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, current.Status, target)
	}
	if !isOperatorRole(role) {
		return model.ReleaseAuthorization{}, ErrForbidden
	}
	if target == "review" {
		current.SubmittedBy = actor
		current.ReviewedBy = ""
		current.ReviewReason = ""
	}
	if target == "approved" || target == "restricted" || target == "revoked" || target == "draft" {
		if !isReviewerRole(role) {
			return model.ReleaseAuthorization{}, ErrForbidden
		}
		if current.SubmittedBy != "" && actor == current.SubmittedBy {
			return model.ReleaseAuthorization{}, ErrSeparationOfDuty
		}
		current.ReviewedBy = actor
		current.ReviewReason = strings.TrimSpace(input.Reason)
	}
	// 提交复核和批准前按关联编号读取部件：hold/retired 时保持原状态并返回编号。
	if target == "review" || target == "approved" {
		blocked, gateErr := s.enforceLinkedPartGate(ctx, &current, target, actor, requestID)
		if gateErr != nil {
			return model.ReleaseAuthorization{}, gateErr
		}
		if blocked {
			return model.ReleaseAuthorization{}, &PartLinkBlockedError{
				PartCode:   current.RelatedCode,
				PartStatus: current.LinkedPartStatus,
				Action:     gateActionLabel(target),
			}
		}
		current.LinkBlockReason = ""
	}
	before := current.Status
	current.Status = target
	current.Version = input.ExpectedVersion + 1
	current.UpdatedAt = time.Now().UTC()
	if err := s.repository.UpdateVersion(ctx, id, input.ExpectedVersion, &current, actor, requestID, "transition", before, strings.TrimSpace(input.Reason)); err != nil {
		return model.ReleaseAuthorization{}, fmt.Errorf("transition 放行授权: %w", err)
	}
	return s.Get(ctx, id)
}

// enforceLinkedPartGate reads the part referenced by RelatedCode. A hold or
// retired part blocks the transition: the attempt is persisted (without touching
// the authorization version) so the release page can re-read the 阻塞原因.
func (s *releaseAuthorizationService) enforceLinkedPartGate(ctx context.Context, current *model.ReleaseAuthorization, target, actor, requestID string) (bool, error) {
	if current.RelatedCode == "" {
		return false, nil
	}
	parts, err := s.parts.FindByCodes(ctx, []string{current.RelatedCode})
	if err != nil {
		return false, fmt.Errorf("read linked part %s: %w", current.RelatedCode, err)
	}
	if len(parts) == 0 {
		return false, nil
	}
	part := parts[0]
	current.LinkedPartStatus = part.Status
	if part.Status != string(constants.PartStateHold) && part.Status != string(constants.PartStateRetired) {
		return false, nil
	}
	reason := fmt.Sprintf("关联部件 %s 当前状态 %s，%s被阻止", part.Code, part.Status, gateActionLabel(target))
	if err := s.repository.RecordLinkBlock(ctx, current.ID, current.Status, reason, actor, requestID); err != nil {
		return false, fmt.Errorf("record link block: %w", err)
	}
	return true, nil
}

func gateActionLabel(target string) string {
	if target == "approved" {
		return "批准"
	}
	return "提交复核"
}

func (s *releaseAuthorizationService) Delete(ctx context.Context, id uint, actor, requestID string) error {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return err
	}
	if current.Status != model.ReleaseAuthorizationInitialStatus {
		return ErrLocked
	}
	if err := s.repository.Delete(ctx, id); err != nil {
		return err
	}
	return s.security.Audit(ctx, actor, requestID, "delete", "ReleaseAuthorization", id, current.Status, "deleted", "soft deleted 放行授权")
}

func (s *releaseAuthorizationService) StatusCounts(ctx context.Context) (map[string]int64, error) {
	return s.repository.CountByStatus(ctx)
}

func validateReleaseAuthorizationBusinessFields(code, name, facility, owner string) error {
	if strings.TrimSpace(code) == "" || strings.TrimSpace(name) == "" || strings.TrimSpace(facility) == "" || strings.TrimSpace(owner) == "" {
		return ErrInvalidInput
	}
	return nil
}

func isOperatorRole(role string) bool {
	return role == model.RoleOperator || role == model.RoleReviewer || role == model.RoleAdmin
}

func isReviewerRole(role string) bool {
	return role == model.RoleReviewer || role == model.RoleAdmin
}
