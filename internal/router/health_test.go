package router

import (
	"errors"
	"fmt"
	"syscall"
	"testing"
)

// A primary failing because this Mac has no route is reported as the
// network being down, until the primary answers again.
func TestHealthSaysNetworkDown(t *testing.T) {
	s := &Server{}
	s.notePrimaryFailure("primary", fmt.Errorf("dial: %w", syscall.ENETUNREACH))
	if h := s.Health(); !h.NetworkDown || h.Failures != 1 {
		t.Fatalf("unreachable: %+v", h)
	}
	s.notePrimaryFailure("primary", errors.New("tls: handshake failure"))
	if s.Health().NetworkDown {
		t.Fatal("an error from the far side is not the network being down")
	}
	s.alertNetworkDown()
	if !s.Health().NetworkDown {
		t.Fatal("the network alert being up counts")
	}
	s.notePrimaryAnswered("primary")
	if s.Health().NetworkDown {
		t.Fatal("an answer ends it")
	}
}
