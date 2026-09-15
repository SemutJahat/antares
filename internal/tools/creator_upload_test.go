package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enowdev/antares/internal/config"
	"github.com/enowdev/antares/internal/creator"
	"github.com/enowdev/antares/internal/store"
)

func TestCreatorUploadCannotBypassStageByOmittingProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ANTARES_HOME", home)
	db, err := store.Open(context.Background(), "memory", "", 1, 5000, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := creator.New(db, config.Path("content-creator"))
	p, err := svc.Create(context.Background(), creator.Project{Title: "Draft", Brief: "Never upload during research"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "private.txt")
	if err := os.WriteFile(path, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	in := Input{Workspace: home, WriteRoots: []string{home}, Deps: &Deps{Store: db}}
	ctx := creator.WithRun(context.Background(), p.ID, "research", "draft", "owner")
	if _, err := resolveUploadPath(ctx, in, "", path); err == nil || !strings.Contains(err.Error(), "upload rejected") {
		t.Fatalf("omitting project bypassed stage guard: %v", err)
	}
}
