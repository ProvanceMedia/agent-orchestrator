package report

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

// Store is the report service's persistence and ownership boundary.
type Store interface {
	GetSession(context.Context, domain.SessionID) (domain.SessionRecord, bool, error)
	CreateReport(context.Context, domain.ReportRecord) (domain.ReportRecord, error)
}

// CreateInput is the validated caller payload before AO-derived ownership and deadlines.
type CreateInput struct {
	SessionID domain.SessionID
	State     domain.ReportState
	Note      string
	Message   string
	Outputs   []domain.ReportOutput
}

// ArtifactRecorder shows a reported artifact in the worker's chat thread. It
// is best effort: it skips what it cannot show and never fails the report.
type ArtifactRecorder interface {
	RecordReportedArtifact(ctx context.Context, id domain.SessionID, reference string)
}

// Service validates and creates durable worker reports.
type Service struct {
	store     Store
	now       func() time.Time
	newID     func() string
	onCreated func(domain.ReportRecord)
	artifacts ArtifactRecorder
}

// Deps configures a Service. Artifacts is optional.
type Deps struct {
	Store     Store
	Now       func() time.Time
	NewID     func() string
	OnCreated func(domain.ReportRecord)
	Artifacts ArtifactRecorder
}

// New constructs a report Service.
func New(d Deps) *Service {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.NewID == nil {
		d.NewID = func() string { return "rpt_" + uuid.NewString() }
	}
	return &Service{store: d.Store, now: d.Now, newID: d.NewID, onCreated: d.OnCreated, artifacts: d.Artifacts}
}

// ListProject returns persisted report facts ordered by created_at then id,
// with each report's outputs in persisted position order. It never claims,
// acknowledges, schedules, or otherwise mutates report delivery state.
func (s *Service) ListProject(ctx context.Context, projectID domain.ProjectID) ([]domain.ReportRecord, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("report: store is required")
	}
	if projectID == "" {
		return nil, apierr.Invalid("REPORT_PROJECT_REQUIRED", "Project id is required", nil)
	}
	reader, ok := s.store.(interface {
		ListReportsByProject(context.Context, domain.ProjectID) ([]domain.ReportRecord, error)
	})
	if !ok {
		return nil, errors.New("report: project reader is required")
	}
	reports, err := reader.ListReportsByProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(reports, func(i, j int) bool {
		if reports[i].CreatedAt.Equal(reports[j].CreatedAt) {
			return reports[i].ID < reports[j].ID
		}
		return reports[i].CreatedAt.Before(reports[j].CreatedAt)
	})
	return reports, nil
}

// Create validates ownership and persists one pending report. Reports are
// coordination claims and never mutate authoritative worker lifecycle state.
func (s *Service) Create(ctx context.Context, input CreateInput) (domain.ReportRecord, error) {
	if s == nil || s.store == nil {
		return domain.ReportRecord{}, errors.New("report: store is required")
	}
	if input.SessionID == "" {
		return domain.ReportRecord{}, apierr.Invalid("INVALID_REPORT_SESSION", "Worker session id is required", nil)
	}
	if err := domain.ValidateReportContent(input.State, input.Note, input.Message, input.Outputs); err != nil {
		return domain.ReportRecord{}, apierr.Invalid("INVALID_REPORT", "Report state, note, message, or outputs are invalid", nil)
	}
	session, ok, err := s.store.GetSession(ctx, input.SessionID)
	if err != nil {
		return domain.ReportRecord{}, err
	}
	if !ok {
		return domain.ReportRecord{}, apierr.NotFound("SESSION_NOT_FOUND", "Unknown session")
	}
	if session.Kind != domain.KindWorker {
		return domain.ReportRecord{}, apierr.Invalid("REPORT_WORKER_REQUIRED", "Reports can only be created by worker sessions", nil)
	}

	now := s.now().UTC()
	var availableAt time.Time
	var settlementDeadline time.Time
	switch input.State {
	case domain.ReportDone:
		settlementDeadline = now.Add(domain.ReportSettlementWindow)
		// The window lets other workers' results join one delivery. When no
		// other worker is busy, nothing can join, so deliver at once.
		if s.noOtherWorkerBusy(ctx, session) {
			settlementDeadline = now
		}
		availableAt = settlementDeadline
	case domain.ReportNeedsInput, domain.ReportStuck:
		availableAt = now
	default:
		availableAt = now.Add(domain.ReportBatchFallback)
	}
	rec := domain.ReportRecord{
		ID: s.newID(), SessionID: session.ID, ProjectID: session.ProjectID,
		State: input.State, Note: input.Note, Message: input.Message, Outputs: input.Outputs,
		CreatedAt: now, DeliveryState: domain.ReportPending, AvailableAt: availableAt,
		SettlementDeadline: settlementDeadline, RepeatCount: 1,
	}
	created, err := s.store.CreateReport(ctx, rec)
	if err != nil {
		return created, err
	}
	if s.onCreated != nil {
		s.onCreated(created)
	}
	// Synchronous on purpose: ao report --done is often the agent's last tool
	// call, so a goroutine would race the turn's completion and could drop the
	// artifact or put it on the next turn.
	if s.artifacts != nil {
		for _, output := range created.Outputs {
			if output.Kind == domain.ReportOutputArtifact {
				s.artifacts.RecordReportedArtifact(ctx, created.SessionID, output.Reference)
			}
		}
	}
	return created, nil
}

// noOtherWorkerBusy reports whether no live worker in the reporter's project,
// other than the reporter, is active. It returns false when the store cannot
// list sessions, so the fixed settlement window stays the safe default.
func (s *Service) noOtherWorkerBusy(ctx context.Context, reporter domain.SessionRecord) bool {
	lister, ok := s.store.(interface {
		ListSessions(context.Context, domain.ProjectID) ([]domain.SessionRecord, error)
	})
	if !ok {
		return false
	}
	sessions, err := lister.ListSessions(ctx, reporter.ProjectID)
	if err != nil {
		return false
	}
	for _, other := range sessions {
		if other.ID == reporter.ID || other.Kind != domain.KindWorker || other.IsTerminated {
			continue
		}
		if other.Activity.State == domain.ActivityActive {
			return false
		}
	}
	return true
}

// CreateReviewResult queues an information report for the project
// orchestrator when a built-in review pass of a worker's PR finishes. The
// report is due at once. The ID comes from the review run, so one run gives at
// most one report. It returns false and creates nothing when the project has
// no active orchestrator, because a report with no receiver stays pending at
// the front of the delivery queue.
func (s *Service) CreateReviewResult(ctx context.Context, workerID domain.SessionID, runID, message string) (bool, error) {
	if s == nil || s.store == nil {
		return false, errors.New("report: store is required")
	}
	if workerID == "" || runID == "" {
		return false, apierr.Invalid("INVALID_REPORT_SESSION", "Worker session id and review run id are required", nil)
	}
	if runes := []rune(message); len(runes) > domain.MaxReportTextCharacters {
		message = string(runes[:domain.MaxReportTextCharacters])
	}
	if err := domain.ValidateReportContent("", "", message, nil); err != nil {
		return false, apierr.Invalid("INVALID_REPORT", "Review result message is invalid", nil)
	}
	session, ok, err := s.store.GetSession(ctx, workerID)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, apierr.NotFound("SESSION_NOT_FOUND", "Unknown session")
	}
	if session.Kind != domain.KindWorker {
		return false, apierr.Invalid("REPORT_WORKER_REQUIRED", "Reports can only be created by worker sessions", nil)
	}
	if !s.hasActiveOrchestrator(ctx, session.ProjectID) {
		return false, nil
	}
	now := s.now().UTC()
	created, err := s.store.CreateReport(ctx, domain.ReportRecord{
		ID: "rpt_review_" + runID, SessionID: session.ID, ProjectID: session.ProjectID,
		Message: message, CreatedAt: now, DeliveryState: domain.ReportPending,
		AvailableAt: now, RepeatCount: 1,
	})
	if err != nil {
		return false, err
	}
	if s.onCreated != nil {
		s.onCreated(created)
	}
	return true, nil
}

// hasActiveOrchestrator uses the same test as the delivery coordinator. It
// returns false when the store cannot list sessions.
func (s *Service) hasActiveOrchestrator(ctx context.Context, projectID domain.ProjectID) bool {
	lister, ok := s.store.(interface {
		ListSessions(context.Context, domain.ProjectID) ([]domain.SessionRecord, error)
	})
	if !ok {
		return false
	}
	sessions, err := lister.ListSessions(ctx, projectID)
	if err != nil {
		return false
	}
	for _, rec := range sessions {
		if rec.Kind == domain.KindOrchestrator && !rec.IsTerminated && rec.Activity.State != domain.ActivityExited {
			return true
		}
	}
	return false
}
