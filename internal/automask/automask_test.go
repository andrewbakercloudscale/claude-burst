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

// Keys are put together here so no scanner takes the test file for a leak.
func TestMasksKeysAndConnectionStrings(t *testing.T) {
	body := strings.Repeat("aB3dE6gH9", 5)
	ant, aws, gh := "sk-"+"ant-api03-"+body, "AKIA"+"IOSFODNN7EXAMPLE", "ghp"+"_"+body[:36]
	pem := "-----BEGIN RSA PRIVATE" + " KEY-----\nMIIEow" + body + "\n-----END RSA PRIVATE" + " KEY-----"
	in := strings.Join([]string{
		"ANTHROPIC_API_KEY=" + ant,
		"aws " + aws + " and github " + gh,
		pem,
		"DATABASE_URL=postgres://app:s3cr3t-Pa55@db.internal:5432/shop?sslmode=require",
		"Server=tcp:sql.example.net;Database=shop;User ID=app;Password=Hunter2Hunter2;Encrypt=true",
		"plain https://example.com/a:b and postgres://app:$DB_PASSWORD@db/shop stay",
		"pip install sk-learn-contrib-something-else stays",
	}, "\n")
	s := NewSession()
	out, hits, _ := s.Mask(in, "tool_result", defaults)
	for _, want := range []string{"ANTHROPIC_API_KEY=[APIKEY-1]", "aws [APIKEY-2] and github [APIKEY-3]", "[PRIVATEKEY-1]",
		"postgres://app:[CONNSTR-1]@db.internal:5432/shop?sslmode=require", "User ID=app;Password=[CONNSTR-2];Encrypt=true",
		"https://example.com/a:b", "postgres://app:$DB_PASSWORD@db/shop", "sk-learn-contrib-something-else"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, gone := range []string{body, "s3cr3t", "Hunter2", "MIIEow"} {
		if strings.Contains(out, gone) {
			t.Errorf("%q not masked:\n%s", gone, out)
		}
	}
	if got := Summary(hits); got != "1 private key, 3 API keys and 2 connection string passwords" {
		t.Errorf("summary = %q", got)
	}
	// Off by default: the value after a name that holds "token".
	out, _, _ = NewSession().Mask("SESSION_TOKEN = \"q7Wz91LmNp04XyTr\"\ntoken = readToken(path)", "user", all)
	if !strings.Contains(out, `SESSION_TOKEN = "[SECRET-1]"`) || !strings.Contains(out, "token = readToken(path)") {
		t.Errorf("secret rule: %q", out)
	}
}
