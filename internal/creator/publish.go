package creator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/enowdev/antares/internal/store"
)

var publicationMu sync.Mutex

func artifactHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func normalizePlatform(platform string) string {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "youtube", "youtube shorts", "shorts":
		return "youtube"
	case "tiktok":
		return "tiktok"
	case "instagram", "instagram reels", "reels":
		return "instagram"
	case "twitter", "x":
		return "x"
	case "facebook":
		return "facebook"
	case "threads":
		return "threads"
	default:
		return strings.ToLower(strings.TrimSpace(platform))
	}
}

func publicationURL(platform, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Hostname() == "" {
		return errors.New("publication requires an observed HTTPS post URL")
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	path := strings.Trim(u.Path, "/")
	okay := false
	switch normalizePlatform(platform) {
	case "youtube":
		okay = (host == "youtube.com" && (strings.HasPrefix(path, "shorts/") || path == "watch" && u.Query().Get("v") != "")) || (host == "youtu.be" && path != "")
	case "tiktok":
		okay = host == "tiktok.com" && strings.Contains(path, "/video/")
	case "instagram":
		okay = host == "instagram.com" && (strings.HasPrefix(path, "reel/") || strings.HasPrefix(path, "p/"))
	case "x":
		okay = (host == "x.com" || host == "twitter.com") && strings.Contains(path, "/status/")
	case "facebook":
		okay = host == "facebook.com" && (strings.Contains(path, "/videos/") || strings.HasPrefix(path, "reel/") || strings.Contains(path, "/posts/"))
	case "threads":
		okay = (host == "threads.net" || host == "threads.com") && strings.Contains(path, "/post/")
	}
	if !okay {
		return errors.New("URL is not a post on the project's platform")
	}
	return nil
}

func (s *Service) PreparePublish(ctx context.Context, id string, approved bool) (Publication, string, error) {
	mu := lockProject(s.root, id)
	mu.Lock()
	defer mu.Unlock()
	p, err := s.load(ctx, id)
	if err != nil {
		return Publication{}, "", err
	}
	if err = s.checkOwner(ctx, p); err != nil {
		return p.Publication, "", err
	}
	if allowed, reason := stageAllows(ctx, "publish"); !allowed {
		return p.Publication, "", errors.New(reason)
	}
	if scope, ok := RunFromContext(ctx); ok && scope.PublishMode == "auto" {
		approved = true
	}
	if p.PublishMode != "auto" && !approved {
		return p.Publication, "", errors.New("publishing is not authorized; enable auto or explicitly approve this upload")
	}
	if p.Publication.Status == "published" {
		return p.Publication, "", errors.New("this video is already published; refusing duplicate upload")
	}
	if p.Publication.Status == "uploading" {
		return p.Publication, "", errors.New("an upload was already prepared; inspect the account and confirm or block it instead of uploading again")
	}
	if p.Publication.Status == "blocked" {
		return p.Publication, "", errors.New("publication is blocked; reconcile the existing attempt before retrying")
	}
	if p.FinalPath == "" {
		return p.Publication, "", errors.New("assemble the final video before publishing")
	}
	path, err := s.Artifact(ctx, id, p.FinalPath)
	if err != nil {
		return p.Publication, "", err
	}
	hash, err := artifactHash(path)
	if err != nil {
		return p.Publication, "", err
	}
	if p.FinalHash != "" && hash != p.FinalHash {
		return p.Publication, "", errors.New("final video changed since assembly; assemble it again")
	}
	account, err := s.db.GetSocialAccount(ctx, p.AccountID)
	if err != nil {
		return p.Publication, "", fmt.Errorf("social account unavailable: %w", err)
	}
	if account.Status != "connected" {
		return p.Publication, "", errors.New("social account is not connected; log in using Social Media first")
	}
	if normalizePlatform(account.Platform) != normalizePlatform(p.Platform) {
		return p.Publication, "", errors.New("social account does not match project platform")
	}
	publicationMu.Lock()
	defer publicationMu.Unlock()
	key := "creator:publication:" + p.AccountID
	holder, err := s.db.GetKV(ctx, key)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return p.Publication, "", err
	}
	if holder != "" && holder != id {
		return p.Publication, "", errors.New("another project has an unresolved upload for this account")
	}
	if err = s.db.SetKV(ctx, key, id); err != nil {
		return p.Publication, "", err
	}
	p.Publication = Publication{Status: "uploading", AccountID: p.AccountID, ArtifactHash: hash, PreparedAt: nowRFC3339()}
	p.Status = "uploading"
	p.Error = ""
	if err = s.save(ctx, &p); err != nil {
		_ = s.db.DeleteKV(ctx, key)
		return p.Publication, "", err
	}
	return p.Publication, path, nil
}

func (s *Service) ConfirmPublish(ctx context.Context, id, postURL, proof, hash string) (Project, error) {
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
	if allowed, reason := stageAllows(ctx, "publish"); !allowed {
		return p, errors.New(reason)
	}
	if p.Publication.Status != "uploading" && p.Publication.Status != "blocked" {
		return p, errors.New("there is no pending upload to confirm")
	}
	if strings.TrimSpace(proof) == "" {
		return p, errors.New("observed browser evidence is required")
	}
	if err = publicationURL(p.Platform, postURL); err != nil {
		return p, err
	}
	if hash == "" || hash != p.Publication.ArtifactHash {
		return p, errors.New("artifact hash does not match pending upload")
	}
	path, err := s.Artifact(ctx, id, p.FinalPath)
	if err != nil {
		return p, err
	}
	current, err := artifactHash(path)
	if err != nil {
		return p, err
	}
	if hash != current {
		return p, errors.New("final video changed during publication")
	}
	p.Publication.Status = "published"
	p.Publication.PostURL = postURL
	p.Publication.Proof = proof
	p.Publication.Error = ""
	p.Publication.PublishedAt = nowRFC3339()
	p.Status = "published"
	p.Error = ""
	if err = s.save(ctx, &p); err != nil {
		return p, err
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
	return p, nil
}

func (s *Service) BlockPublish(ctx context.Context, id, reason string) (Project, error) {
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
	if strings.TrimSpace(reason) == "" {
		return p, errors.New("blocked reason required")
	}
	if p.Publication.Status == "published" {
		return p, errors.New("published video cannot be marked blocked")
	}
	p.Publication.Status = "blocked"
	p.Publication.Error = reason
	p.Status = "blocked"
	p.Error = reason
	if err = s.save(ctx, &p); err != nil {
		return p, err
	}
	return p, nil
}
