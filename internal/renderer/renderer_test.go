package renderer

import (
	"context"
	"errors"
	"testing"
)

func TestNoopRenderer(t *testing.T) {
	r := New()
	if r.Available() {
		t.Errorf("Available() = true, want false for MVP no-op renderer")
	}
	html, err := r.Render(context.Background(), "https://example.com")
	if html != nil {
		t.Errorf("Render() html = %v, want nil", html)
	}
	if !errors.Is(err, ErrRendererUnavailable) {
		t.Errorf("Render() err = %v, want ErrRendererUnavailable", err)
	}
}
