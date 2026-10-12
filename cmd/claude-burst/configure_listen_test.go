package main

import (
	"strings"
	"testing"
)

// `configure` stored any listen address it was given. Neither listener has a
// login, so one that is not loopback is refused unless the flag that
// acknowledges it was passed, and the refusal names that flag.
func TestConfigureRefusesANonLoopbackListenUnlessAcknowledged(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:7777", "localhost:7788", "[::1]:7777"} {
		for _, flagName := range []string{"--listen", "--admin-listen"} {
			if err := checkListen(flagName, addr, false); err != nil {
				t.Errorf("%s %s refused: %v", flagName, addr, err)
			}
		}
	}
	for _, addr := range []string{"0.0.0.0:7777", ":7777", "192.168.1.20:7777", "[::]:7777", "7777"} {
		for _, flagName := range []string{"--listen", "--admin-listen"} {
			err := checkListen(flagName, addr, false)
			if err == nil {
				t.Errorf("%s %s was accepted without --allow-non-loopback-listen", flagName, addr)
				continue
			}
			for _, want := range []string{flagName, addr, "--allow-non-loopback-listen"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal does not mention %q: %v", want, err)
				}
			}
			if err := checkListen(flagName, addr, true); err != nil {
				t.Errorf("%s %s refused although acknowledged: %v", flagName, addr, err)
			}
		}
	}
}

// The two refusals must say what each listener really does on a wider bind,
// which is not the same thing: the gateway answers other machines, and the
// dashboard goes on refusing every peer that is not this Mac (admin.guard).
func TestListenRefusalTellsTheSameStoryAsTheGuards(t *testing.T) {
	gw := checkListen("--listen", "0.0.0.0:7777", false).Error()
	if !strings.Contains(gw, "no login") || strings.Contains(gw, "only connections from this Mac") {
		t.Errorf("gateway refusal: %s", gw)
	}
	admin := checkListen("--admin-listen", "0.0.0.0:7788", false).Error()
	if !strings.Contains(admin, "only connections from this Mac") {
		t.Errorf("dashboard refusal must say a wider bind is not served: %s", admin)
	}
}
