package agent

import (
	"context"
	"errors"
	"sync"
)

var (
	ErrSessionBusy  = errors.New("a turn is already running for this session")
	ErrSessionLimit = errors.New("concurrent session limit reached")
)

// PreparedRun reserves capacity before a host starts streaming. Close also
// releases an unused reservation, for example when the client disconnects.
type PreparedRun struct {
	agent   *Agent
	request Request
	ctx     context.Context
	cancel  context.CancelFunc
	once    sync.Once
	started bool
	closed  bool
	mu      sync.Mutex
}

func (a *Agent) Prepare(ctx context.Context, req Request) (*PreparedRun, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.SessionID == "" {
		req.SessionID = newID("ses")
	}
	runCtx, cancel := context.WithCancel(ctx)
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, exists := a.active[req.SessionID]; exists {
		cancel()
		return nil, ErrSessionBusy
	}
	top := req.Depth == 0
	if a.available == nil {
		a.available = make(chan struct{})
	}
	if top && a.config().MaxConcurrentSessions > 0 && a.topActive >= a.config().MaxConcurrentSessions {
		cancel()
		return nil, ErrSessionLimit
	}
	if a.active == nil {
		a.active = make(map[string]context.CancelFunc)
	}
	a.active[req.SessionID] = cancel
	if top {
		a.topActive++
	}
	return &PreparedRun{agent: a, request: req, ctx: runCtx, cancel: cancel}, nil
}

func (p *PreparedRun) SessionID() string { return p.request.SessionID }

func (p *PreparedRun) Close() {
	p.mu.Lock()
	p.closed = true
	running := p.started
	p.cancel()
	p.mu.Unlock()
	if !running {
		p.release()
	}
}

func (p *PreparedRun) release() {
	p.once.Do(func() {
		p.cancel()
		p.agent.mu.Lock()
		delete(p.agent.active, p.request.SessionID)
		if p.request.Depth == 0 {
			p.agent.topActive--
		}
		if p.agent.available != nil {
			close(p.agent.available)
		}
		p.agent.available = make(chan struct{})
		p.agent.mu.Unlock()
	})
}

func (p *PreparedRun) Run(emit Emit) (*Result, error) {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return nil, ErrSessionBusy
	}
	if p.closed {
		p.mu.Unlock()
		return nil, context.Canceled
	}
	p.started = true
	p.mu.Unlock()
	defer p.release()
	if err := p.ctx.Err(); err != nil {
		return nil, err
	}
	return p.agent.run(p.ctx, p.request, emit)
}

func (a *Agent) Run(ctx context.Context, req Request, emit Emit) (*Result, error) {
	prepared, err := a.Prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	return prepared.Run(emit)
}

// RunQueued is for autonomous continuations that must retain their context
// while other sessions occupy capacity. User requests use nonblocking Run.
func (a *Agent) RunQueued(ctx context.Context, req Request, emit Emit) (*Result, error) {
	for {
		a.mu.Lock()
		if a.available == nil {
			a.available = make(chan struct{})
		}
		changed := a.available
		a.mu.Unlock()
		prepared, err := a.Prepare(ctx, req)
		if err == nil {
			return prepared.Run(emit)
		}
		if !errors.Is(err, ErrSessionBusy) && !errors.Is(err, ErrSessionLimit) {
			return nil, err
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
