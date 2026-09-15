package creator

import (
	"context"
	"errors"
	"strings"
)

// Reset discards generated outputs only after an explicit operator decision.
// A pending provider job cannot be forgotten because that could bill twice.
func (s *Service) Reset(ctx context.Context, id, kind, target string) (Project, error) {
	mu := lockProject(s.root, id)
	mu.Lock()
	defer mu.Unlock()
	p, err := s.load(ctx, id)
	if err != nil {
		return p, err
	}
	if _, scoped := RunFromContext(ctx); scoped {
		return p, errors.New("reset requires an interactive operator decision")
	}
	if err = s.checkOwner(ctx, p); err != nil {
		return p, err
	}
	if p.Publication.Status == "uploading" || p.Publication.Status == "blocked" || p.Publication.Status == "published" {
		return p, errors.New("publication must be resolved or a new project created before resetting media")
	}
	affected := map[int]bool{}
	refIndex := -1
	switch kind {
	case "reset_reference":
		for i, r := range p.References {
			if r.ID == target {
				refIndex = i
			}
		}
		if refIndex < 0 {
			return p, errors.New("unknown reference")
		}
		for i, shot := range p.Shots {
			for _, refID := range shot.ReferenceIDs {
				if refID == target {
					affected[i] = true
				}
			}
		}
	case "reset_shot":
		for i, shot := range p.Shots {
			if shot.ID == target {
				affected[i] = true
			}
		}
		if len(affected) == 0 {
			return p, errors.New("unknown shot")
		}
	default:
		return p, errors.New("unknown reset action")
	}
	for i := range p.Shots {
		if i > 0 && affected[i-1] && p.Shots[i].Continuity == "continue" {
			affected[i] = true
		}
		if affected[i] {
			shot := p.Shots[i]
			if shot.VideoJobID != "" && shot.Status != "ready" && shot.Status != "failed" {
				return p, errors.New("poll pending video jobs before resetting their shots")
			}
		}
	}
	if refIndex >= 0 {
		clearReference(&p.References[refIndex])
	}
	for i := range affected {
		clearShot(&p.Shots[i])
	}
	p.FinalPath = ""
	p.FinalHash = ""
	p.Status = "draft"
	p.Error = ""
	if err = s.save(ctx, &p); err != nil {
		return p, err
	}
	return p, nil
}

func (s *Service) ReconcileNotPublished(ctx context.Context, id, proof string) (Project, error) {
	if strings.TrimSpace(proof) == "" {
		return Project{}, errors.New("record how you verified the upload was not published")
	}
	if _, scoped := RunFromContext(ctx); scoped {
		return Project{}, errors.New("upload reconciliation requires the operator")
	}
	mu := lockProject(s.root, id)
	mu.Lock()
	defer mu.Unlock()
	p, err := s.load(ctx, id)
	if err != nil {
		return p, err
	}
	if err = s.checkOwner(ctx, p); err != nil {
		return p, err
	}
	if p.Publication.Status != "uploading" && p.Publication.Status != "blocked" {
		return p, errors.New("no uncertain publication to reconcile")
	}
	publicationMu.Lock()
	defer publicationMu.Unlock()
	key := "creator:publication:" + p.AccountID
	holder, _ := s.db.GetKV(ctx, key)
	if holder == id {
		if err = s.db.DeleteKV(ctx, key); err != nil {
			return p, err
		}
	}
	p.Publication = Publication{Status: "not_started", Proof: "Operator verified not published: " + proof}
	p.Status = "assembled"
	p.Error = ""
	if err = s.save(ctx, &p); err != nil {
		return p, err
	}
	return p, nil
}
