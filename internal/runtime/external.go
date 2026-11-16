package runtime

import (
	"context"
	"errors"
	"sync"
)

// External "runs" something that's already running on this computer, such
// as a dev server on localhost:3000: an instance is just its address, so the
// router and xu526 tunnel can reach it. Nothing is started or stopped.
type External struct{}

func (External) Name() string { return "external" }

func (External) Start(ctx context.Context, s Spec) (*Instance, error) {
	if s.Upstream == "" {
		return nil, errors.New("no address to forward to")
	}
	ch := make(chan struct{})
	in := &Instance{ID: "local", Addr: s.Upstream, Exited: ch}
	externalStops.Store(in, sync.OnceFunc(func() { close(ch) }))
	return in, nil
}

var externalStops sync.Map // *Instance → func() closing Exited

func (External) Stop(ctx context.Context, in *Instance) error {
	if f, ok := externalStops.LoadAndDelete(in); ok {
		f.(func())()
	}
	return nil
}

func (External) Cleanup(context.Context) error { return nil }
