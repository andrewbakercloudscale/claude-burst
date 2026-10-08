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

func TestMasksTokensCookiesWebhooksAndSignedLinks(t *testing.T) {
	body := strings.Repeat("aB3dE6gH9", 5)
	jwt := "eyJ" + body[:20] + ".eyJ" + body[:30] + "." + body[:40]
	in := strings.Join([]string{
		"token " + jwt,
		"Authorization: Bearer " + body[:32],
		"Authorization: Basic " + "dXNlcjpwYXNzd29yZDEyMw==",
		"curl -s -u deploy:Tr0ub4dor3 https://ci.example.com/job",
		"Set-Cookie: session=" + body[:24] + "; Path=/; HttpOnly",
		"post to https://hooks.slack" + ".com/services/T00000000/B00000000/" + body[:24],
		"https://bucket.s3.amazonaws.com/report.pdf?X-Amz-Expires=3600&X-Amz-Signature=" + strings.Repeat("0a1b", 16),
		"twilio SK" + strings.Repeat("0a1b", 8) + " and telegram 123456789:AA" + body[:33],
		"docker run -u 1000:1000 img, Bearer token_goes_here_in_your_request and cookie: jar stay",
		"import com.example.MyVeryLongNamespaceXy.Module.SomeExtremelyLongClassNameHereOk stays",
		"API_URL=abcDEF123456 stays outside a .env file",
	}, "\n")
	out, hits, _ := NewSession().Mask(in, "tool_result", defaults)
	for _, want := range []string{"token [TOKEN-1]", "Authorization: Bearer [TOKEN-2]", "Authorization: Basic [BASICAUTH-1]",
		"-u deploy:[BASICAUTH-2] https://ci.example.com/job", "Set-Cookie: [COOKIE-1]", "https://hooks.slack.com/services/[WEBHOOK-1]",
		"report.pdf?X-Amz-Expires=3600&X-Amz-Signature=[SIGNATURE-1]", "twilio [APIKEY-1] and telegram [APIKEY-2]",
		"-u 1000:1000", "Bearer token_goes_here_in_your_request", "cookie: jar", "SomeExtremelyLongClassNameHereOk", "API_URL=abcDEF123456"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, body[:20]) || strings.Contains(out, "Tr0ub4dor3") || strings.Contains(out, "dXNlcjpw") {
		t.Errorf("a value got through:\n%s", out)
	}
	if got := Summary(hits); got != "2 API keys, 2 access tokens, 2 basic auth credentials, 1 cookie, 1 webhook URL and 1 signed URL" {
		t.Errorf("summary = %q", got)
	}
}

// In a .env file every value that could be a secret goes, by where it sits.
func TestMasksTheValuesOfAnEnvFile(t *testing.T) {
	in := "     1\tPORT=3000\n     2\tNODE_ENV=production\n     3\tDEBUG=true\n     4\tAPI_URL=https://api.example.com/v1\n" +
		"     5\tDB_HOST=db1.internal.example\n     6\texport DATADOG_KEY=\"0a1b2c3d4e5f60718293a4b5c6d7e8f9\"\n     7\tAPP_SECRET=xYz12345AbC # rotate\n     8\tCERT=/etc/ssl/app1.pem"
	out, hits, _ := NewSession().MaskEnvFile(in, "tool_result", defaults)
	for _, want := range []string{"PORT=3000", "NODE_ENV=production", "DEBUG=true", "API_URL=https://api.example.com/v1", "DB_HOST=db1.internal.example",
		`export DATADOG_KEY="[ENV-1]"`, "APP_SECRET=[ENV-2] # rotate", "CERT=/etc/ssl/app1.pem"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if len(hits) != 2 {
		t.Errorf("hits = %d, want 2", len(hits))
	}
}
