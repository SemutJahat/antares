package server

import "github.com/enowdev/antares/internal/skills"

// currentSkills returns the live skill manager: the agent owns it after a
// reload (rt.reload rebuilds it), so a per-operation snapshot from the agent
// is authoritative. Tests that construct a Server with no agent still get the
// seeded Options.Skills.
func (s *Server) currentSkills() *skills.Manager {
	if s.agent != nil {
		if m := s.agent.Skills(); m != nil {
			return m
		}
	}
	return s.skills
}
