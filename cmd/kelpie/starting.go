package main

import (
	"context"
	"errors"

	"github.com/metalmatze/kelpie/internal/control"
	"github.com/metalmatze/kelpie/internal/core"
)

// startingHandler serves the control socket while the daemon is still
// starting. Status answers at once with nothing, which is all a liveness check
// needs; commands that act on the daemon's state wait until it is set.
type startingHandler struct {
	ready chan struct{}
	d     *daemon
}

func (s *startingHandler) set(d *daemon) {
	s.d = d
	close(s.ready)
}

func (s *startingHandler) wait(ctx context.Context) error {
	select {
	case <-s.ready:
		return nil
	case <-ctx.Done():
		return errors.New("kelpie daemon is still starting")
	}
}

func (s *startingHandler) Watch(ctx context.Context, req control.WatchRequest) (control.WatchResponse, error) {
	if err := s.wait(ctx); err != nil {
		return control.WatchResponse{}, err
	}
	return s.d.Watch(ctx, req)
}

func (s *startingHandler) Unwatch(ctx context.Context, req control.WatchRequest) error {
	if err := s.wait(ctx); err != nil {
		return err
	}
	return s.d.Unwatch(ctx, req)
}

func (s *startingHandler) Remind(ctx context.Context, req control.RemindRequest) (core.Reminder, error) {
	if err := s.wait(ctx); err != nil {
		return core.Reminder{}, err
	}
	return s.d.Remind(ctx, req)
}

func (s *startingHandler) Unremind(ctx context.Context, id string) error {
	if err := s.wait(ctx); err != nil {
		return err
	}
	return s.d.Unremind(ctx, id)
}

func (s *startingHandler) Status(ctx context.Context) control.Status {
	select {
	case <-s.ready:
		return s.d.Status(ctx)
	default:
		return control.Status{}
	}
}
