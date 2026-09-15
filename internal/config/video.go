package config

// VideoGen configures the async video generation used by the Content Creator
// workflow. It points at an OpenAI-compatible /videos endpoint (Sora-2 style):
// POST /videos creates a job, GET /videos/{id} polls it, GET /videos/{id}/content
// downloads the finished MP4. When Enabled is false, no network call is ever
// made — media tools refuse to run.
type VideoGen struct {
	Enabled  bool   `yaml:"enabled" json:"enabled"`
	Provider string `yaml:"provider" json:"provider"` // a configured provider id, or "openai"
	Model    string `yaml:"model" json:"model"`
	BaseURL  string `yaml:"base_url" json:"base_url"`
	APIKey   string `yaml:"api_key" json:"api_key"`
	Size     string `yaml:"size" json:"size"`       // WxH, e.g. "720x1280"
	Seconds  int    `yaml:"seconds" json:"seconds"` // clip duration in seconds, default 8
}
