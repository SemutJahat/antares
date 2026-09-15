package creator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/enowdev/antares/internal/store"
)

func testService(t *testing.T) (*Service, Project) {
	t.Helper()
	db, err := store.Open(context.Background(), "memory", "", 1, 5000, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	svc := New(db, t.TempDir())
	p, err := svc.Create(context.Background(), Project{Title: "Sequence", Brief: "Two scenes with one character", TargetSeconds: 8, References: []Reference{{ID: "hero", Kind: "character", Name: "Hero", Prompt: "red coat"}}, Shots: []Shot{{ID: "one", Prompt: "walk", ReferenceIDs: []string{"hero"}, DurationSeconds: 4, Continuity: "cut"}, {ID: "two", Prompt: "turn", DurationSeconds: 4, Continuity: "continue"}}})
	if err != nil {
		t.Fatal(err)
	}
	return svc, p
}

func TestProjectRejectsStaleEditorAndInvalidatesContinuations(t *testing.T) {
	svc, p := testService(t)
	ctx := context.Background()
	for i := range p.Shots {
		p.Shots[i].Status = "ready"
		p.Shots[i].VideoPath = "generated.mp4"
		p.Shots[i].LastFramePath = "last.png"
	}
	p.FinalPath = "final.mp4"
	p.FinalHash = "digest"
	if err := svc.save(ctx, &p); err != nil {
		t.Fatal(err)
	}
	stale := p
	next := p
	next.Shots = append([]Shot(nil), p.Shots...)
	next.Shots[0].Prompt = "run"
	saved, err := svc.Update(ctx, p.ID, next)
	if err != nil {
		t.Fatal(err)
	}
	if saved.FinalPath != "" || saved.Shots[1].VideoPath != "" || saved.Shots[0].VideoPath != "" {
		t.Fatal("changed scene retained stale clips or final output")
	}
	if _, err = svc.Update(ctx, p.ID, stale); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale update: %v", err)
	}
}

func TestProjectRunOwnershipAndRestartRecovery(t *testing.T) {
	svc, p := testService(t)
	ctx := context.Background()
	if _, err := svc.BeginRun(ctx, p.ID, "produce", "draft", "session-one"); err != nil {
		t.Fatal(err)
	}
	if _, err := New(svc.db, svc.root).BeginRun(ctx, p.ID, "plan", "draft", "session-two"); !errors.Is(err, ErrRunInProgress) {
		t.Fatalf("duplicate active run: %v", err)
	}
	active, _ := svc.Get(ctx, p.ID)
	if _, err := svc.Update(ctx, p.ID, active); !errors.Is(err, ErrRunInProgress) {
		t.Fatalf("external editor clobbered run: %v", err)
	}
	owned := WithRun(ctx, p.ID, "produce", "draft", "session-one")
	active.Caption = "a caption"
	if _, err := svc.Update(owned, p.ID, active); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.EndRun(ctx, p.ID, "session-one", errors.New("provider failed")); err != nil {
		t.Fatal(err)
	}
	stored, _ := svc.Get(ctx, p.ID)
	if stored.LastRunError != "provider failed" || stored.RunStatus != "error" {
		t.Fatalf("lost run failure: %+v", stored)
	}
	if _, err := svc.BeginRun(ctx, p.ID, "produce", "draft", "session-two"); err != nil {
		t.Fatal(err)
	}
	_, _ = svc.EndRun(ctx, p.ID, "session-two", nil)
}

func TestArtifactRejectsUnregisteredAndEscapedFiles(t *testing.T) {
	svc, p := testService(t)
	ctx := context.Background()
	dir, _ := svc.projectDir(p.ID)
	path := filepath.Join(dir, "final.mp4")
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Artifact(ctx, p.ID, path); err == nil {
		t.Fatal("unregistered file exposed")
	}
	p.FinalPath = path
	if err := svc.save(ctx, &p); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := svc.Artifact(ctx, p.ID, path); err != nil || got != resolved {
		t.Fatalf("registered artifact: %s %v", got, err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	_ = os.WriteFile(outside, []byte("secret"), 0600)
	_ = os.Remove(path)
	if err := os.Symlink(outside, path); err != nil {
		t.Skip(err)
	}
	if _, err := svc.Artifact(ctx, p.ID, path); err == nil {
		t.Fatal("symlink exposed file outside project")
	}
}
