package keychain

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"strings"
	"time"
)

// securityPath is the Keychain CLI; a variable so tests run a stub instead
// of reading or writing the real login Keychain.
var securityPath = "/usr/bin/security"

// SetSecurityPathForTesting points the package at another security
// executable (a stub) and returns a func that restores the real one. For
// other packages' tests, which must never read or write the login Keychain.
func SetSecurityPathForTesting(path string) (restore func()) {
	old := securityPath
	securityPath = path
	return func() { securityPath = old }
}

// timeout bounds every security call. A locked login Keychain can leave it
// waiting on a dialog nobody sees (the gateway runs headless under
// launchd), and the caller, a gateway starting or a dashboard request,
// would wait with it. A variable so tests can shorten it.
var timeout = 10 * time.Second

// errTimedOut names the likely cause, since a bare "signal: killed" does not.
var errTimedOut = errors.New("the Keychain did not answer in time (is the login Keychain locked?)")

// security runs the Keychain CLI with args, bounded by timeout.
func security(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, securityPath, args...)
	// Kill only reaches security itself; WaitDelay stops a child holding
	// its output open from keeping CombinedOutput waiting anyway.
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return out, errTimedOut
	}
	return out, err
}

func account() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if v := os.Getenv("USER"); v != "" {
		return v
	}
	return "claude-burst"
}

func Store(service, value string) error {
	if value == "" {
		return fmt.Errorf("empty key")
	}
	if out, err := security("add-generic-password", "-U", "-a", account(), "-s", service, "-w", value); err != nil {
		return fmt.Errorf("security add-generic-password: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Load returns the secret for service, checking envVar first and falling
// back to macOS Keychain. envVar is caller-specified (rather than hardcoded
// here) because different providers use different env vars for the same
// override convenience -- e.g. AWS_BEARER_TOKEN_BEDROCK for the "bedrock"
// service, TOGETHER_API_KEY for a "claude-burst-together" service. A single
// hardcoded env var here would silently hand one provider's secret to
// another provider's Load call.
func Load(service, envVar string) (string, error) {
	if v := os.Getenv(envVar); v != "" {
		return v, nil
	}
	out, err := security("find-generic-password", "-a", account(), "-s", service, "-w")
	if errors.Is(err, errTimedOut) {
		return "", fmt.Errorf("reading %q: %w", service, err)
	}
	if err != nil {
		return "", fmt.Errorf("key not found in %s or macOS Keychain (service %q)", envVar, service)
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return "", fmt.Errorf("key is empty (service %q)", service)
	}
	return v, nil
}

// Delete removes the stored secret. Nothing stored is success: the goal is
// "no secret in the Keychain", and it already holds. Any other failure is
// returned, so a caller never reports a password removed that is still there.
func Delete(service string) error {
	out, err := security("delete-generic-password", "-a", account(), "-s", service)
	if err == nil {
		return nil
	}
	// security exits 44 (errSecItemNotFound) when there is nothing to delete.
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 44 {
		return nil
	}
	return fmt.Errorf("deleting %q from the Keychain: %v: %s", service, err, strings.TrimSpace(string(out)))
}

// Info describes a stored secret without revealing it.
//
// Source is load-bearing rather than decorative: Load checks the
// environment first and falls back to the Keychain, so "a key exists" has
// two very different meanings. An env-var key lives only in the processes
// that inherited it and disappears on the next login; a Keychain key is
// what a saved configuration actually relies on. A UI that reported only a
// green tick would show the same thing either way, and a user who had just
// saved a key would have no way to tell whether their save landed or
// whether it was an unrelated env var answering all along.
type Info struct {
	Present bool
	// Source is "environment" or "keychain", empty when Present is false.
	Source string
	// Modified is when the Keychain entry was last written, in local time.
	// Zero for an environment key (which has no such record), and zero if
	// the date cannot be parsed -- callers must treat it as unknown rather
	// than as the epoch.
	Modified time.Time
}

// Describe reports whether a secret is available for service, and from
// where, without ever reading its value.
//
// Deliberately drops Load's -w flag: the admin UI needs to answer "is a key
// stored for this provider, and when did it last change?" and nothing more,
// and a value that is never fetched cannot end up in a log line, an error
// string, or a JSON response by accident. It checks envVar the same way
// Load does, so its answer is what Load would actually find rather than a
// Keychain-only view -- a key supplied purely through the environment must
// not read as missing.
func Describe(service, envVar string) Info {
	if os.Getenv(envVar) != "" {
		return Info{Present: true, Source: "environment"}
	}
	out, err := security("find-generic-password", "-a", account(), "-s", service)
	if err != nil {
		return Info{}
	}
	return Info{Present: true, Source: "keychain", Modified: parseKeychainDate(out)}
}

// keychainDate matches the modification-date attribute in `security
// find-generic-password` output, which is printed as hex rather than text:
//
//	"mdat"<timedate>=0x32303236...5A00  "20260908123034Z\000"
//
// The hex form is parsed rather than the quoted one because the trailing
// literal is an escaped C string; the hex is unambiguous.
var keychainDate = regexp.MustCompile(`"mdat"<timedate>=0x([0-9A-Fa-f]+)`)

// parseKeychainDate extracts the entry's last-modified time as LOCAL time.
// Keychain records it in UTC ("...Z"); returning it unconverted would put a
// UTC timestamp next to this machine's local-time clocks, which is how a
// perfectly fresh save comes to look hours stale.
func parseKeychainDate(out []byte) time.Time {
	m := keychainDate.FindSubmatch(out)
	if m == nil {
		return time.Time{}
	}
	raw, err := hex.DecodeString(string(m[1]))
	if err != nil {
		return time.Time{}
	}
	t, err := time.Parse("20060102150405Z", strings.TrimRight(string(raw), "\x00"))
	if err != nil {
		return time.Time{}
	}
	return t.Local()
}
