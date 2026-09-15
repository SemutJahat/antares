package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/enowdev/antares/internal/agent"
	"github.com/enowdev/antares/internal/config"
	"github.com/enowdev/antares/internal/creator"
	"github.com/enowdev/antares/internal/media"
)

func (s *Server) creatorService() *creator.Service {
	return creator.New(s.db, config.Path("content-creator"))
}

func (s *Server) handleCreatorList(w http.ResponseWriter, r *http.Request) {
	projects, err := s.creatorService().List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": projects})
}

func (s *Server) handleCreatorGet(w http.ResponseWriter, r *http.Request) {
	p, err := s.creatorService().Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleCreatorCreate(w http.ResponseWriter, r *http.Request) {
	if s.requireDashboardPassword(w, r) {
		return
	}
	var p creator.Project
	if err := decodeBody(r, &p); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	p, err := s.creatorService().Create(r.Context(), p)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) handleCreatorUpdate(w http.ResponseWriter, r *http.Request) {
	if s.requireDashboardPassword(w, r) {
		return
	}
	var p creator.Project
	if err := decodeBody(r, &p); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	p, err := s.creatorService().Update(r.Context(), r.PathValue("id"), p)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleCreatorAction(w http.ResponseWriter, r *http.Request) {
	if s.requireDashboardPassword(w, r) {
		return
	}
	var in struct {
		Action    string `json:"action"`
		TargetID  string `json:"target_id"`
		Confirmed bool   `json:"confirmed"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if in.Action == "reset_reference" || in.Action == "reset_shot" {
		if !in.Confirmed {
			writeError(w, http.StatusBadRequest, errors.New("reset requires explicit confirmation"))
			return
		}
		p, err := s.creatorService().Reset(r.Context(), r.PathValue("id"), in.Action, in.TargetID)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, p)
		return
	}
	switch in.Action {
	case "generate_reference", "generate_keyframe", "generate_video", "poll_video", "assemble":
	default:
		writeError(w, http.StatusBadRequest, errors.New("unsupported media action"))
		return
	}
	p, err := s.creatorService().Execute(r.Context(), s.config(), r.PathValue("id"), in.Action, in.TargetID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleCreatorReconcile(w http.ResponseWriter, r *http.Request) {
	if s.requireDashboardPassword(w, r) {
		return
	}
	var in struct {
		Proof        string `json:"proof"`
		NotPublished bool   `json:"not_published"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !in.NotPublished {
		writeError(w, http.StatusBadRequest, errors.New("verify the upload was not published before allowing another attempt"))
		return
	}
	p, err := s.creatorService().ReconcileNotPublished(r.Context(), r.PathValue("id"), in.Proof)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleCreatorArtifact(w http.ResponseWriter, r *http.Request) {
	path, err := s.creatorService().Artifact(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeFile(w, r, path)
}

func (s *Server) handleCreatorRun(w http.ResponseWriter, r *http.Request) {
	if s.requireDashboardPassword(w, r) {
		return
	}
	if s.agent == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("agent is unavailable"))
		return
	}
	var in struct {
		Stage       string `json:"stage"`
		PublishMode string `json:"publish_mode"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	switch in.Stage {
	case "research", "plan", "produce", "publish", "full":
	default:
		writeError(w, http.StatusBadRequest, errors.New("stage must be research, plan, produce, publish, or full"))
		return
	}
	if in.PublishMode == "" {
		in.PublishMode = "draft"
	}
	if in.PublishMode != "draft" && in.PublishMode != "auto" {
		writeError(w, http.StatusBadRequest, errors.New("publish_mode must be draft or auto"))
		return
	}
	if in.Stage == "publish" && in.PublishMode != "auto" {
		writeError(w, http.StatusBadRequest, errors.New("publishing requires explicit auto-publish authorization"))
		return
	}
	id := r.PathValue("id")
	svc := s.creatorService()
	p, err := svc.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	sid := "ses_creator_" + hex.EncodeToString(random[:])
	ctx := creator.WithRun(context.Background(), id, in.Stage, in.PublishMode, sid)
	prepared, err := s.agent.Prepare(ctx, agent.Request{
		SessionID: sid,
		Role:      "content-creator", Platform: "web", Workspace: config.Path("content-creator", id),
		Message:     fmt.Sprintf("Run the %s stage for Content Creator project %s (%s). Load its saved state with content_creator; complete this stage and persist the result. Publish mode: %s. Do not repeat completed generation or publication.", in.Stage, id, p.Title, in.PublishMode),
		SystemExtra: "Work only on the requested Content Creator project and stage. Research/plan never generate or publish. Produce stops at a final draft. Publish only when explicitly authorized with publish_mode=auto. Use saved reference assets for visual consistency; record real source URLs for trend research. If credentials, browser login, or approval are unavailable, record a blocked reason instead of claiming success.",
	})
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	if _, err := svc.BeginRun(r.Context(), id, in.Stage, in.PublishMode, sid); err != nil {
		prepared.Close()
		writeError(w, http.StatusConflict, err)
		return
	}
	if err := s.db.SetKV(r.Context(), "role:"+sid, "content-creator"); err != nil {
		prepared.Close()
		_, _ = svc.EndRun(context.Background(), id, sid, err)
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	lr := newLiveRun()
	s.hub.put(sid, lr)
	go func() {
		var runErr error
		defer func() {
			if rec := recover(); rec != nil {
				runErr = fmt.Errorf("Content Creator run panicked: %v", rec)
				lr.publish(agent.Event{Type: agent.EventError, Err: runErr.Error()})
				lr.publish(agent.Event{Type: agent.EventDone})
			}
			prepared.Close()
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = svc.EndRun(cleanup, id, sid, runErr)
			if lock, ok := any(s.social).(interface{ Release(string) }); ok && s.social != nil {
				lock.Release(sid)
			}
			lr.finish()
			s.hub.remove(sid, lr)
		}()
		_, runErr = prepared.Run(func(e agent.Event) error { lr.publish(e); return nil })
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"session_id": sid})
}

type creatorMediaSettings struct {
	Enabled  bool   `json:"enabled"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	BaseURL  string `json:"base_url"`
	Size     string `json:"size"`
	Seconds  int    `json:"seconds,omitempty"`
	HasKey   bool   `json:"has_key"`
}

func (s *Server) handleCreatorSettings(w http.ResponseWriter, r *http.Request) {
	cfg := s.config()
	image := creatorMediaSettings{Enabled: cfg.ImageGen.Enabled, Provider: cfg.ImageGen.Provider, Model: cfg.ImageGen.Model, BaseURL: cfg.ImageGen.BaseURL, Size: cfg.ImageGen.Size, HasKey: cfg.ImageGen.APIKey != ""}
	video := creatorMediaSettings{Enabled: cfg.VideoGen.Enabled, Provider: cfg.VideoGen.Provider, Model: cfg.VideoGen.Model, BaseURL: cfg.VideoGen.BaseURL, Size: cfg.VideoGen.Size, Seconds: cfg.VideoGen.Seconds, HasKey: cfg.VideoGen.APIKey != ""}
	for _, p := range []*creatorMediaSettings{&image, &video} {
		name := p.Provider
		if name == "" {
			name = "openai"
		}
		if provider, ok := cfg.Providers[name]; ok {
			if p.BaseURL == "" {
				p.BaseURL = provider.BaseURL
			}
			p.HasKey = p.HasKey || provider.APIKey != ""
		}
	}
	_, ffmpegErr := exec.LookPath("ffmpeg")
	_, ffprobeErr := exec.LookPath("ffprobe")
	writeJSON(w, http.StatusOK, map[string]any{"image": image, "video": video, "ffmpeg": ffmpegErr == nil, "ffprobe": ffprobeErr == nil})
}

func (s *Server) handleCreatorSaveSettings(w http.ResponseWriter, r *http.Request) {
	if s.requireDashboardPassword(w, r) {
		return
	}
	var in struct {
		Image map[string]any `json:"image"`
		Video map[string]any `json:"video"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.creatorConfigMu.Lock()
	defer s.creatorConfigMu.Unlock()
	cfg, err := config.Reload()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	cfg = cfg.Clone()
	for prefix, fields := range map[string]map[string]any{"image_gen": in.Image, "video_gen": in.Video} {
		for key, value := range fields {
			switch key {
			case "enabled", "provider", "model", "base_url", "api_key", "size":
			case "seconds":
				if prefix != "video_gen" {
					writeError(w, http.StatusBadRequest, errors.New("seconds applies only to video"))
					return
				}
			default:
				writeError(w, http.StatusBadRequest, fmt.Errorf("unsupported media setting %q", key))
				return
			}
			if key == "api_key" {
				if v, ok := value.(string); ok && (strings.TrimSpace(v) == "" || strings.Contains(v, "••••")) {
					continue
				}
			}
			if err := cfg.SetPath(prefix+"."+key, value); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
		}
	}
	for _, raw := range []string{cfg.ImageGen.BaseURL, cfg.VideoGen.BaseURL} {
		if raw != "" {
			if err := s.validateCustomProviderBaseURL(r.Context(), raw); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
		}
	}
	if cfg.VideoGen.Seconds <= 0 || cfg.VideoGen.Seconds > 120 {
		writeError(w, http.StatusBadRequest, errors.New("video seconds must be between 1 and 120"))
		return
	}
	if cfg.ImageGen.Enabled {
		if _, err := media.ImageEndpoint(cfg); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	if cfg.VideoGen.Enabled {
		if _, err := media.VideoEndpoint(cfg); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	if err := config.Save(cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.applyReload(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.handleCreatorSettings(w, r)
}
