package operr_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/hypersequent/oddk/internal/operr"
)

func TestMarkersClassifyWithoutChangingMessage(t *testing.T) {
	err := operr.NotFoundf("instance not found: %s", "my-app")
	if err.Error() != "instance not found: my-app" {
		t.Errorf("marker leaked into message: %q", err.Error())
	}
	if !errors.Is(err, operr.ErrNotFound) {
		t.Error("NotFoundf error should match ErrNotFound")
	}
	if errors.Is(err, operr.ErrConflict) {
		t.Error("NotFoundf error should not match ErrConflict")
	}
}

func TestMarkerSurvivesWrapping(t *testing.T) {
	inner := operr.Conflictf("boom")
	wrapped := fmt.Errorf("get instance: %w", inner)
	if !errors.Is(wrapped, operr.ErrConflict) {
		t.Error("marker should survive fmt.Errorf wrapping")
	}
	if wrapped.Error() != "get instance: boom" {
		t.Errorf("unexpected wrapped message: %q", wrapped.Error())
	}
}

func TestMarkerPreservesInnerWrappedError(t *testing.T) {
	cause := errors.New("root cause")
	err := operr.Invalidf("bad thing: %w", cause)
	if !errors.Is(err, cause) {
		t.Error("%w inside a marked error should remain matchable")
	}
	if !errors.Is(err, operr.ErrInvalid) {
		t.Error("marker should also match")
	}
}
