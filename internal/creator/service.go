package creator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/enowdev/antares/internal/store"
)

var ErrRunInProgress = errors.New("content project has an active run")
var ErrRevisionConflict = errors.New("project changed; reload before saving")
var projectLocks sync.Map
var activeRuns sync.Map

const projectPrefix = "creator:project:"

type Service struct {
	db   store.Store
	root string
}

func New(db store.Store, root string) *Service {
	absolute, err := filepath.Abs(root)
	if err == nil {
		root = absolute
	}
	return &Service{db: db, root: root}
}

func lockProject(root, id string) *sync.Mutex {
	value, _ := projectLocks.LoadOrStore(filepath.Join(root, id), &sync.Mutex{})
	return value.(*sync.Mutex)
}

func validID(id string) bool {
	if len(id) == 0 || len(id) > 100 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func (s *Service) projectDir(id string) (string, error) {
	if !validID(id) {
		return "", errors.New("invalid project id")
	}
	return filepath.Join(s.root, id), nil
}

func (s *Service) load(ctx context.Context, id string) (Project, error) {
	if !validID(id) {
		return Project{}, errors.New("invalid project id")
	}
	if s.db == nil {
		return Project{}, errors.New("project storage unavailable")
	}
	raw, err := s.db.GetKV(ctx, projectPrefix+id)
	if err != nil {
		return Project{}, err
	}
	if raw == "" {
		return Project{}, store.ErrNotFound
	}
	var p Project
	if err = json.Unmarshal([]byte(raw), &p); err != nil {
		return Project{}, fmt.Errorf("corrupt project state: %w", err)
	}
	if p.ID != id {
		return Project{}, errors.New("project identity mismatch")
	}
	if p.RunStatus == "running" {
		if _, live := activeRuns.Load(filepath.Join(s.root, id)); !live {
			p.RunStatus = "error"
			p.LastRunError = "The previous run was interrupted. Resume the stage to continue from saved artifacts."
		}
	}
	normalizeSlices(&p)
	return p, nil
}

func normalizeSlices(p *Project) {
	if p.References == nil {
		p.References = []Reference{}
	}
	if p.Shots == nil {
		p.Shots = []Shot{}
	}
	if p.Research == nil {
		p.Research = []Trend{}
	}
	if p.Ideas == nil {
		p.Ideas = []Idea{}
	}
}

func (s *Service) save(ctx context.Context, p *Project) error {
	p.Revision++
	p.UpdatedAt = nowRFC3339()
	normalizeSlices(p)
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return s.db.SetKV(ctx, projectPrefix+p.ID, string(b))
}

func (s *Service) List(ctx context.Context) ([]Project, error) {
	if s.db == nil {
		return nil, errors.New("project storage unavailable")
	}
	rows, err := s.db.ListKV(ctx, projectPrefix)
	if err != nil {
		return nil, err
	}
	out := make([]Project, 0, len(rows))
	for _, raw := range rows {
		var p Project
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return nil, err
		}
		if p.RunStatus == "running" {
			if _, live := activeRuns.Load(filepath.Join(s.root, p.ID)); !live {
				p.RunStatus = "error"
				p.LastRunError = "The previous run was interrupted. Resume from saved artifacts."
			}
		}
		normalizeSlices(&p)
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out, nil
}

func (s *Service) Get(ctx context.Context, id string) (Project, error) { return s.load(ctx, id) }

func (s *Service) Create(ctx context.Context, p Project) (Project, error) {
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		return Project{}, err
	}
	p.ID = "video_" + hex.EncodeToString(id[:])
	p.Revision = 0
	p.CreatedAt = nowRFC3339()
	p.Status = "draft"
	p.Error = ""
	p.FinalPath = ""
	p.FinalHash = ""
	p.RunSessionID = ""
	p.RunStage = ""
	p.RunPublishMode = ""
	p.RunStatus = "idle"
	p.LastRunError = ""
	p.RunStartedAt = ""
	p.RunFinishedAt = ""
	p.Publication = Publication{Status: "not_started"}
	if p.PublishMode == "" {
		p.PublishMode = "draft"
	}
	if p.Size == "" {
		p.Size = "720x1280"
	}
	if p.TargetSeconds == 0 {
		p.TargetSeconds = 24
	}
	if err := validateProject(p); err != nil {
		return Project{}, err
	}
	for i := range p.References {
		clearReference(&p.References[i])
	}
	for i := range p.Shots {
		clearShot(&p.Shots[i])
	}
	dir, _ := s.projectDir(p.ID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return Project{}, err
	}
	if err := s.save(ctx, &p); err != nil {
		return Project{}, err
	}
	return p, nil
}

func clearReference(r *Reference) { r.Path = ""; r.Status = "draft"; r.Error = "" }
func clearShot(s *Shot) {
	s.KeyframePath = ""
	s.VideoJobID = ""
	s.VideoPath = ""
	s.LastFramePath = ""
	s.Status = "draft"
	s.Error = ""
	s.Progress = 0
	s.PromptHash = ""
	s.ProviderSnapshot = ""
}

func (s *Service) checkOwner(ctx context.Context, p Project) error {
	if owner, ok := activeRuns.Load(filepath.Join(s.root, p.ID)); ok {
		rc, scoped := RunFromContext(ctx)
		if !scoped || rc.SessionID != owner.(string) {
			return ErrRunInProgress
		}
	}
	if rc, ok := RunFromContext(ctx); ok && rc.ProjectID != p.ID {
		return errors.New("run is scoped to a different project")
	}
	return nil
}

func (s *Service) Update(ctx context.Context, id string, next Project) (Project, error) {
	mu := lockProject(s.root, id)
	mu.Lock()
	defer mu.Unlock()
	old, err := s.load(ctx, id)
	if err != nil {
		return Project{}, err
	}
	if err = s.checkOwner(ctx, old); err != nil {
		return old, err
	}
	if next.Revision != old.Revision {
		return old, ErrRevisionConflict
	}
	next.ID = id
	if err = validateProject(next); err != nil {
		return old, err
	}
	// Caller edits plans, never generated paths, provider jobs, run ownership or publication evidence.
	next.CreatedAt = old.CreatedAt
	next.Revision = old.Revision
	next.Status = old.Status
	next.Error = old.Error
	next.RunSessionID = old.RunSessionID
	next.RunStage = old.RunStage
	next.RunPublishMode = old.RunPublishMode
	next.RunStatus = old.RunStatus
	next.LastRunError = old.LastRunError
	next.RunStartedAt = old.RunStartedAt
	next.RunFinishedAt = old.RunFinishedAt
	next.FinalPath = old.FinalPath
	next.FinalHash = old.FinalHash
	next.Publication = old.Publication
	refs := map[string]Reference{}
	for _, ref := range old.References {
		refs[ref.ID] = ref
	}
	changedRefs := map[string]bool{}
	for i := range next.References {
		r := &next.References[i]
		previous, exists := refs[r.ID]
		if exists && old.Style == next.Style && old.Size == next.Size && previous.Prompt == r.Prompt && previous.Kind == r.Kind && previous.Name == r.Name {
			r.Path = previous.Path
			r.Status = previous.Status
			r.Error = previous.Error
		} else {
			clearReference(r)
			changedRefs[r.ID] = true
		}
	}
	nextIDs := map[string]bool{}
	for _, shot := range next.Shots {
		nextIDs[shot.ID] = true
	}
	for _, shot := range old.Shots {
		if !nextIDs[shot.ID] && (shot.Status == "submission_unknown" || shot.VideoJobID != "" && shot.Status != "ready" && shot.Status != "failed") {
			return old, errors.New("cannot remove a shot with a pending or uncertain video job")
		}
	}
	shots := map[string]Shot{}
	for _, shot := range old.Shots {
		shots[shot.ID] = shot
	}
	invalidated := old.Style != next.Style || old.Size != next.Size || len(old.Shots) != len(next.Shots)
	precedingChanged := false
	for i := range next.Shots {
		shot := &next.Shots[i]
		previous, exists := shots[shot.ID]
		changed := !exists || previous.Prompt != shot.Prompt || previous.Notes != shot.Notes || previous.DurationSeconds != shot.DurationSeconds || previous.Continuity != shot.Continuity || !reflect.DeepEqual(previous.ReferenceIDs, shot.ReferenceIDs) || old.Style != next.Style || old.Size != next.Size
		if i >= len(old.Shots) || old.Shots[i].ID != shot.ID {
			changed = true
		}
		for _, refID := range shot.ReferenceIDs {
			if changedRefs[refID] {
				changed = true
			}
		}
		if precedingChanged && shot.Continuity == "continue" {
			changed = true
		}
		if changed {
			if exists && (previous.Status == "submission_unknown" || previous.VideoJobID != "" && previous.Status != "ready" && previous.Status != "failed") {
				return old, errors.New("cannot edit a shot with a pending or uncertain video job; resolve it first")
			}
			clearShot(shot)
			invalidated = true
		} else {
			shot.KeyframePath = previous.KeyframePath
			shot.VideoJobID = previous.VideoJobID
			shot.VideoPath = previous.VideoPath
			shot.LastFramePath = previous.LastFramePath
			shot.Status = previous.Status
			shot.Error = previous.Error
			shot.Progress = previous.Progress
			shot.PromptHash = previous.PromptHash
			shot.ProviderSnapshot = previous.ProviderSnapshot
		}
		precedingChanged = changed
	}
	if old.Publication.Status != "not_started" && old.Publication.Status != "" && (invalidated || next.Caption != old.Caption || next.AccountID != old.AccountID || next.Platform != old.Platform) {
		return old, errors.New("publication must be reconciled or a new project created before editing")
	}
	if invalidated {
		next.FinalPath = ""
		next.FinalHash = ""
		next.Status = "draft"
		next.Error = ""
		next.Publication = Publication{Status: "not_started"}
	}
	if !invalidated && (next.AccountID != old.AccountID || next.Platform != old.Platform) && old.Publication.Status == "published" {
		return old, errors.New("create a separate project to publish to another account")
	}
	if err = s.save(ctx, &next); err != nil {
		return old, err
	}
	return next, nil
}

func validateProject(p Project) error {
	if strings.TrimSpace(p.Title) == "" || strings.TrimSpace(p.Brief) == "" {
		return errors.New("title and brief are required")
	}
	if p.PublishMode != "draft" && p.PublishMode != "auto" {
		return errors.New("publish_mode must be draft or auto")
	}
	if p.TargetSeconds < 1 || p.TargetSeconds > 3600 {
		return errors.New("target_seconds must be between 1 and 3600")
	}
	var w, h int
	if n, _ := fmt.Sscanf(p.Size, "%dx%d", &w, &h); n != 2 || w < 16 || h < 16 || w > 4096 || h > 4096 || w%2 != 0 || h%2 != 0 || p.Size != fmt.Sprintf("%dx%d", w, h) {
		return errors.New("size must be even WIDTHxHEIGHT dimensions between 16 and 4096")
	}
	if len(p.Shots) > 100 || len(p.References) > 50 || len(p.Research) > 200 || len(p.Ideas) > 50 {
		return errors.New("project exceeds item limits")
	}
	ids := map[string]bool{}
	for _, r := range p.References {
		if !validID(r.ID) || ids[r.ID] || r.Prompt == "" {
			return errors.New("references require unique ids and prompts")
		}
		ids[r.ID] = true
		switch r.Kind {
		case "character", "setting", "style", "prop":
		default:
			return errors.New("invalid reference kind")
		}
	}
	shots := map[string]bool{}
	for i, shot := range p.Shots {
		if !validID(shot.ID) || shots[shot.ID] || shot.Prompt == "" {
			return errors.New("shots require unique ids and prompts")
		}
		shots[shot.ID] = true
		if shot.DurationSeconds < 1 || shot.DurationSeconds > 120 {
			return errors.New("shot duration must be between 1 and 120 seconds")
		}
		if shot.Continuity != "cut" && shot.Continuity != "continue" {
			return errors.New("continuity must be cut or continue")
		}
		if i == 0 && shot.Continuity == "continue" {
			return errors.New("first shot cannot continue a previous clip")
		}
		for _, id := range shot.ReferenceIDs {
			if !ids[id] {
				return fmt.Errorf("unknown reference %s", id)
			}
		}
	}
	for _, trend := range p.Research {
		u, err := url.Parse(trend.URL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
			return errors.New("research evidence requires an HTTP(S) source URL")
		}
		if strings.TrimSpace(trend.Title) == "" {
			return errors.New("research evidence requires a title")
		}
		if _, err := time.Parse(time.RFC3339, trend.ObservedAt); err != nil {
			return errors.New("research evidence requires an RFC3339 observation time")
		}
		for _, value := range trend.Metrics {
			if value < 0 {
				return errors.New("research metrics cannot be negative")
			}
		}
	}
	ideas := map[string]bool{}
	selected := 0
	for _, idea := range p.Ideas {
		if !validID(idea.ID) || ideas[idea.ID] || strings.TrimSpace(idea.Title) == "" {
			return errors.New("ideas require unique ids and titles")
		}
		ideas[idea.ID] = true
		if idea.Selected {
			selected++
		}
	}
	if selected > 1 {
		return errors.New("select only one idea per video project")
	}
	return nil
}

func (s *Service) Artifact(ctx context.Context, id, path string) (string, error) {
	p, err := s.load(ctx, id)
	if err != nil {
		return "", err
	}
	dir, err := s.projectDir(id)
	if err != nil {
		return "", err
	}
	requested := path
	if !filepath.IsAbs(requested) {
		requested = filepath.Join(dir, requested)
	}
	requested = filepath.Clean(requested)
	registered := []string{p.FinalPath}
	for _, r := range p.References {
		registered = append(registered, r.Path)
	}
	for _, shot := range p.Shots {
		registered = append(registered, shot.KeyframePath, shot.VideoPath, shot.LastFramePath)
	}
	found := false
	for _, v := range registered {
		if v == "" {
			continue
		}
		if !filepath.IsAbs(v) {
			v = filepath.Join(dir, v)
		}
		if filepath.Clean(v) == requested {
			found = true
			break
		}
	}
	if !found {
		return "", errors.New("artifact is not registered to this project")
	}
	rootReal, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	rootBase, baseErr := filepath.EvalSymlinks(s.root)
	if baseErr != nil {
		return "", baseErr
	}
	if filepath.Dir(rootReal) != rootBase {
		return "", errors.New("project directory escapes content root")
	}
	real, err := filepath.EvalSymlinks(requested)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(rootReal, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", errors.New("artifact escapes project directory")
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("artifact is not a regular file")
	}
	return real, nil
}

func (s *Service) BeginRun(ctx context.Context, id, stage, mode, sid string) (Project, error) {
	mu := lockProject(s.root, id)
	mu.Lock()
	defer mu.Unlock()
	p, err := s.load(ctx, id)
	if err != nil {
		return p, err
	}
	switch stage {
	case "research", "plan", "produce", "publish", "full":
	default:
		return p, errors.New("invalid creator stage")
	}
	if mode != "draft" && mode != "auto" {
		return p, errors.New("invalid publish mode")
	}
	if stage == "publish" && mode != "auto" {
		return p, errors.New("publish requires explicit auto authorization")
	}
	if sid == "" {
		return p, errors.New("run session id required")
	}
	key := filepath.Join(s.root, id)
	if _, loaded := activeRuns.LoadOrStore(key, sid); loaded {
		return p, ErrRunInProgress
	}
	p.RunSessionID = sid
	p.RunStage = stage
	p.RunPublishMode = mode
	p.RunStatus = "running"
	p.RunStartedAt = nowRFC3339()
	p.RunFinishedAt = ""
	p.LastRunError = ""
	if err := s.save(ctx, &p); err != nil {
		activeRuns.Delete(key)
		return p, err
	}
	return p, nil
}

func (s *Service) EndRun(ctx context.Context, id, sid string, runErr error) (Project, error) {
	mu := lockProject(s.root, id)
	mu.Lock()
	defer mu.Unlock()
	p, err := s.load(ctx, id)
	if err != nil {
		return p, err
	}
	if p.RunSessionID != sid {
		return p, nil
	}
	key := filepath.Join(s.root, id)
	if owner, ok := activeRuns.Load(key); ok && owner == sid {
		activeRuns.Delete(key)
	}
	p.RunStatus = "idle"
	p.RunFinishedAt = nowRFC3339()
	if runErr != nil {
		p.RunStatus = "error"
		p.LastRunError = runErr.Error()
	} else if p.Error != "" {
		// The model returning text is not proof that the requested stage finished.
		p.RunStatus = "error"
		p.LastRunError = p.Error
	}
	if p.RunStatus == "idle" {
		missing := ""
		switch p.RunStage {
		case "research":
			if len(p.Research) == 0 {
				missing = "Research ended without saved source evidence."
			}
		case "plan":
			if len(p.Ideas) == 0 || len(p.Shots) == 0 || len(p.References) == 0 {
				missing = "Planning ended without ideas, references, and shots."
			}
		case "produce", "full":
			if p.FinalPath == "" {
				missing = "Production stopped before the final video was assembled. Resume from saved shots."
			}
		case "publish":
			if p.Publication.Status != "published" {
				missing = "Publishing ended without a verified post URL."
			}
		}
		if p.RunStage == "full" && p.RunPublishMode == "auto" && p.Publication.Status != "published" {
			missing = "Full run ended without a verified publication."
		}
		if missing != "" {
			p.RunStatus = "error"
			p.LastRunError = missing
		}
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err = s.save(writeCtx, &p); err != nil {
		return p, err
	}
	return p, nil
}
