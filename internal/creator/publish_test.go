package creator

import (
	"context"
	"github.com/enowdev/antares/internal/store"
	"os"
	"path/filepath"
	"testing"
)

func TestPublishStageCannotEscapeDraftAuthorization(t *testing.T) {
	svc, p := testService(t)
	for _, stage := range []string{"research", "plan", "produce", "full", "publish"} {
		ctx := WithRun(context.Background(), p.ID, stage, "draft", "run")
		if _, _, err := svc.PreparePublish(ctx, p.ID, true); err == nil {
			t.Fatalf("%s escaped draft mode with approved=true", stage)
		}
	}
}

func TestPublicationURLsRequirePlatformPosts(t *testing.T) {
	cases := []struct {
		platform, url string
		valid         bool
	}{
		{"youtube", "https://www.youtube.com/shorts/abc", true},
		{"youtube", "https://youtube.com/watch?v=abc", true},
		{"instagram", "https://instagram.com/reel/abc/", true},
		{"tiktok", "https://www.tiktok.com/@creator/video/123", true},
		{"youtube", "https://youtube.com.evil.example/shorts/abc", false},
		{"instagram", "https://instagram.com/", false},
		{"instagram", "https://evil@instagram.com/reel/abc", false},
		{"tiktok", "https://tiktok.com/upload", false},
	}
	for _, c := range cases {
		if err := publicationURL(c.platform, c.url); (err == nil) != c.valid {
			t.Errorf("%s: %v", c.url, err)
		}
	}
}

type publicationStore struct{ store.Store }

func (s publicationStore) GetSocialAccount(context.Context, string) (*store.SocialAccount, error) {
	return &store.SocialAccount{ID: "account", Platform: "youtube", Status: "connected"}, nil
}

func TestPublicationReservesAndReconcilesOneUpload(t *testing.T) {
	svc, p := testService(t)
	ctx := context.Background()
	svc.db = publicationStore{svc.db}
	dir, _ := svc.projectDir(p.ID)
	path := filepath.Join(dir, "final.mp4")
	if err := os.WriteFile(path, []byte("fixture-video"), 0600); err != nil {
		t.Fatal(err)
	}
	p.FinalPath = path
	p.FinalHash, _ = artifactHash(path)
	p.AccountID = "account"
	p.Platform = "youtube"
	p.PublishMode = "auto"
	if err := svc.save(ctx, &p); err != nil {
		t.Fatal(err)
	}
	reservation, upload, err := svc.PreparePublish(ctx, p.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if reservation.Status != "uploading" || upload == "" {
		t.Fatal("upload reservation missing")
	}
	if _, _, err := svc.PreparePublish(ctx, p.ID, false); err == nil {
		t.Fatal("duplicate upload allowed")
	}
	if _, err := svc.ConfirmPublish(ctx, p.ID, "https://youtube.com/shorts/observed", "", reservation.ArtifactHash); err == nil {
		t.Fatal("publication without evidence accepted")
	}
	if _, err := svc.BlockPublish(ctx, p.ID, "connection lost after submit"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.PreparePublish(ctx, p.ID, false); err == nil {
		t.Fatal("uncertain upload automatically retried")
	}
	confirmed, err := svc.ConfirmPublish(ctx, p.ID, "https://youtube.com/shorts/observed", "Account page shows the uploaded video", reservation.ArtifactHash)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.Publication.Status != "published" {
		t.Fatal("confirmation not persisted")
	}
	if _, _, err := svc.PreparePublish(ctx, p.ID, false); err == nil {
		t.Fatal("published video uploaded twice")
	}
}
