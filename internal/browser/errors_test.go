package browser

import (
	"errors"
	"testing"
)

func TestBrowserErrorCodeAndMessage(t *testing.T) {
	err := NewBrowserError(CodeElementNotFound, "ref e12 not found")
	var be *BrowserError
	if !errors.As(err, &be) {
		t.Fatalf("expected *BrowserError, got %T", err)
	}
	if be.Code != CodeElementNotFound {
		t.Fatalf("code = %q, want %q", be.Code, CodeElementNotFound)
	}
	if be.Error() != "ELEMENT_NOT_FOUND: ref e12 not found" {
		t.Fatalf("Error() = %q", be.Error())
	}
}
