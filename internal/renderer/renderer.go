// Package renderer defines the JS-rendering interface. The MVP build ships a
// no-op renderer; JS rendering is deferred and the default binary never imports
// a headless engine.
package renderer

import (
	"context"
	"errors"
)

// ErrRendererUnavailable is returned by the MVP build's no-op renderer.
var ErrRendererUnavailable = errors.New("rabbot: JS renderer not available in this build")

type Renderer interface {
	Available() bool
	Render(ctx context.Context, url string) (html []byte, err error)
}

type noop struct{}

// New returns the MVP no-op renderer.
func New() Renderer { return noop{} }

func (noop) Available() bool { return false }

func (noop) Render(ctx context.Context, url string) ([]byte, error) {
	return nil, ErrRendererUnavailable
}
