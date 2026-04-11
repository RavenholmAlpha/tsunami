package control

import (
	"context"
	"fmt"
)

// Middleware transforms or validates a normalized Snapshot before it reaches
// the runtime user store.
type Middleware interface {
	Name() string
	Apply(ctx context.Context, snapshot *Snapshot) error
}

// MiddlewareFunc adapts a function into Middleware.
type MiddlewareFunc struct {
	name string
	fn   func(context.Context, *Snapshot) error
}

// NewMiddlewareFunc creates a named middleware from a function.
func NewMiddlewareFunc(name string, fn func(context.Context, *Snapshot) error) MiddlewareFunc {
	return MiddlewareFunc{name: name, fn: fn}
}

// Name returns the middleware name.
func (m MiddlewareFunc) Name() string {
	if m.name == "" {
		return "middleware"
	}
	return m.name
}

// Apply executes the middleware function.
func (m MiddlewareFunc) Apply(ctx context.Context, snapshot *Snapshot) error {
	if m.fn == nil {
		return nil
	}
	return m.fn(ctx, snapshot)
}

// Pipeline connects one adapter with a middleware chain.
type Pipeline struct {
	adapter     Adapter
	middlewares []Middleware
}

// NewPipeline creates a new adapter pipeline.
func NewPipeline(adapter Adapter, middlewares ...Middleware) (*Pipeline, error) {
	if adapter == nil {
		return nil, fmt.Errorf("control: adapter is required")
	}
	return &Pipeline{adapter: adapter, middlewares: middlewares}, nil
}

// FetchSnapshot fetches, normalizes, and validates a snapshot.
func (p *Pipeline) FetchSnapshot(ctx context.Context) (*Snapshot, error) {
	if p == nil || p.adapter == nil {
		return nil, fmt.Errorf("control: pipeline has no adapter")
	}

	snapshot, err := p.adapter.FetchSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	if snapshot == nil {
		return nil, fmt.Errorf("control: adapter %s returned nil snapshot", p.adapter.Name())
	}
	if snapshot.Source == "" {
		snapshot.Source = p.adapter.Name()
	}

	if err := ApplyMiddleware(ctx, snapshot, p.middlewares...); err != nil {
		return nil, err
	}
	return snapshot, nil
}

// ApplyMiddleware applies a middleware chain to a snapshot.
func ApplyMiddleware(ctx context.Context, snapshot *Snapshot, middlewares ...Middleware) error {
	for _, middleware := range middlewares {
		if middleware == nil {
			continue
		}
		if err := middleware.Apply(ctx, snapshot); err != nil {
			return fmt.Errorf("control: middleware %s: %w", middleware.Name(), err)
		}
	}
	return nil
}

// NormalizeUsers fills stable display fields that adapters may omit.
func NormalizeUsers() Middleware {
	return NewMiddlewareFunc("normalize-users", func(ctx context.Context, snapshot *Snapshot) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		for _, user := range snapshot.Users {
			if user == nil {
				continue
			}
			if user.Name == "" && user.ID != "" {
				user.Name = user.ID
			}
			if user.ID == "" && user.Name != "" {
				user.ID = user.Name
			}
		}
		return nil
	})
}

// ValidateUsers verifies that every user can authenticate and no auth hash is
// duplicated in the same snapshot.
func ValidateUsers() Middleware {
	return NewMiddlewareFunc("validate-users", func(ctx context.Context, snapshot *Snapshot) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		seen := make(map[[32]byte]string, len(snapshot.Users))
		for idx, user := range snapshot.Users {
			if user == nil {
				return fmt.Errorf("user[%d] is nil", idx)
			}
			hash, err := user.AuthHash()
			if err != nil {
				return err
			}
			identity := user.Identity()
			if prev, ok := seen[hash]; ok {
				return fmt.Errorf("duplicate auth hash for users %q and %q", prev, identity)
			}
			seen[hash] = identity
		}
		return nil
	})
}
