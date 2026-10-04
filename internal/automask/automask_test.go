package automask

import (
	"strings"
	"testing"
)

func all(*Rule) bool { return true }

func defaults(r *Rule) bool { return r.Default }

func TestMasksTheDefaultRulesAndLeavesLookalikes(t *testing.T) {
	s := NewSession()
	in := strings.Join([]string{
		"card 4111 1111 1111 1111 and amex 378282246310005",
		"not a card 1234567812345678",            // fails Luhn
		"SA ID 8001015009087, not 8013015009087", // month 13
		"SSN 123-45-6789, not 000-12-3456",
		"NI AB123456C, not GB123456A",
		"IBAN GB82WEST12345698765432, not GB00WEST12345698765432",
		"MRZ L898902C36UTO7408122F1204159",
		"mail a@b.com and phone +27821234567 stay", // off by default
	}, "\n")
	out, hits, changed := s.Mask(in, "user", defaults)
	if !changed {
		t.Fatal("nothing masked")
	}
	for _, want := range []string{"[CARD-1 ...1111]", "[CARD-2 ...0005]", "[SAID-1]", "[SSN-1]", "[NINO-1]", "[IBAN-1]", "[PASSPORT-1]",
		"1234567812345678", "8013015009087", "000-12-3456", "GB123456A", "GB00WEST12345698765432", "a@b.com", "+27821234567"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, gone := range []string{"4111 1111", "8001015009087", "123-45-6789", "AB123456C", "GB82WEST"} {
		if strings.Contains(out, gone) {
			t.Errorf("%q not masked:\n%s", gone, out)
		}
	}
	if len(hits) != 7 {
		t.Errorf("hits = %d, want 7", len(hits))
	}
}

func TestSameValueSameMaskAndNoSecondHit(t *testing.T) {
	s := NewSession()
	a, h1, _ := s.Mask("pay 4111-1111-1111-1111", "user", all)
	b, h2, _ := s.Mask("again 4111111111111111 and 5555 5555 5555 4444", "tool_result", all)
	if !strings.Contains(a, "[CARD-1 ...1111]") || !strings.Contains(b, "[CARD-1 ...1111]") || !strings.Contains(b, "[CARD-2 ...4444]") {
		t.Fatalf("masks not stable: %q / %q", a, b)
	}
	if len(h1) != 1 || len(h2) != 1 || h2[0].Mask != "[CARD-2 ...4444]" {
		t.Fatalf("hits = %+v / %+v; only a new value is a hit", h1, h2)
	}
	if got := Summary(append(h1, h2...)); got != "2 credit card numbers" {
		t.Errorf("summary = %q", got)
	}
}

func TestOptInRules(t *testing.T) {
	s := NewSession()
	out, hits, _ := s.Mask("mail me at jo@example.co.za, call 082 123 4567, account no: 1234567890, host 10.0.0.256 or 192.168.1.10", "user", all)
	for _, want := range []string{"[EMAIL-1]", "[PHONE-1]", "account no: [BANKACC-1]", "10.0.0.256", "[IP-1]"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	if got := Summary(hits); got != "1 email address, 1 phone number, 1 bank account number and 1 IP address" {
		t.Errorf("summary = %q", got)
	}
}
