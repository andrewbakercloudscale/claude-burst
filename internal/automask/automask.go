// Package automask finds personal data in text and replaces it with a mask
// before a request leaves the Mac. It never rejects: a refused request
// breaks the turn, a masked one does not. See docs/design/automask.md.
package automask

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Rule is one kind of personal data.
type Rule struct {
	ID      string // config key, e.g. "card"
	Name    string // for people, e.g. "Credit card number"
	One     string // "credit card number", for a count of one
	Plural  string // "credit card numbers"
	Prefix  string // mask prefix, e.g. "CARD"
	Default bool   // on unless the config says otherwise
	Note    string // why it is off by default, or what it checks
	re      *regexp.Regexp
	group   int // submatch to mask; 0 is the whole match, -1 the first that matched
	valid   func(string) bool
	keep4   bool // show the last four digits in the mask
	exact   bool // the value is told apart as written: a dash is part of a key
	envOnly bool // tried only on the output of a tool call that names a .env file
}

// Rules is every rule, in the order they are tried. A later rule never sees
// text an earlier one masked.
var Rules = []*Rule{
	// Keys go first: a later rule must not mask a run of digits inside one.
	{ID: "privatekey", One: "private key", Name: "Private key block", Plural: "private keys", Prefix: "PRIVATEKEY", Default: true,
		Note: "a PEM block from BEGIN to END PRIVATE KEY (RSA, EC, OpenSSH, PGP)",
		re:   regexp.MustCompile(`-----BEGIN (?:[A-Z]+ )*PRIVATE KEY(?: BLOCK)?-----[\s\S]+?-----END (?:[A-Z]+ )*PRIVATE KEY(?: BLOCK)?-----`), exact: true},
	{ID: "apikey", One: "API key", Name: "API key", Plural: "API keys", Prefix: "APIKEY", Default: true,
		Note:  "keys with a known shape: Anthropic, OpenAI, AWS, GitHub, GitLab, Google, Slack, Stripe, npm, PyPI, Hugging Face, SendGrid, Twilio, Mailgun, DigitalOcean, Shopify, Databricks, Discord and Telegram bots",
		re:    regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{20,}|(?:AKIA|ASIA)[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,}|glpat-[A-Za-z0-9_-]{20,}|AIza[0-9A-Za-z_-]{35}|xox[abeprs]-[A-Za-z0-9-]{10,}|[sr]k_(?:live|test)_[A-Za-z0-9]{16,}|npm_[A-Za-z0-9]{36}|hf_[A-Za-z0-9]{30,}|SG\.[A-Za-z0-9_-]{22}\.[A-Za-z0-9_-]{43}|SK[0-9a-f]{32}|key-[0-9a-f]{32}|do[opr]_v1_[a-f0-9]{64}|shp(?:at|ca|pa|ss)_[a-fA-F0-9]{32}|[MNO][A-Za-z0-9_-]{23,25}\.[A-Za-z0-9_-]{6}\.[A-Za-z0-9_-]{27,}|\d{8,10}:AA[A-Za-z0-9_-]{33}|pypi-AgEIcHlwaS5vcmc[A-Za-z0-9_-]{50,}|dapi[a-f0-9]{32})`),
		valid: validAPIKey, exact: true},
	{ID: "connstr", One: "connection string password", Name: "Connection string password", Plural: "connection string passwords", Prefix: "CONNSTR", Default: true,
		Note: "the password in scheme://user:password@host, and Password=, Pwd=, AccountKey= or SharedAccessKey= after a semicolon; the rest of the string is left to read",
		re:   regexp.MustCompile(`(?i)(?:\b[a-z][a-z0-9+.-]*://[^\s:/@"'<>]+:([^\s@/"'<>]{3,})@|;\s*(?:password|pwd|accountkey|sharedaccesskey|sharedaccesssignature)\s*=\s*([^;\s"']{3,}))`), group: -1,
		valid: validConnPassword, exact: true},
	{ID: "token", One: "access token", Name: "Bearer token or JWT", Plural: "access tokens", Prefix: "TOKEN", Default: true,
		Note: "a JSON Web Token anywhere, and what follows Bearer or an Authorization header",
		re:   regexp.MustCompile(`(\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,})|(?i:\bauthorization["']?\s*[:=]\s*["']?(?:bearer|token)\s+([A-Za-z0-9._~+/=-]{16,}))|(?i:\bbearer\s+([A-Za-z0-9._~+/=-]{20,}))`), group: -1,
		valid: validToken, exact: true},
	{ID: "basicauth", One: "basic auth credential", Name: "Basic auth credentials", Plural: "basic auth credentials", Prefix: "BASICAUTH", Default: true,
		Note: "what follows Authorization: Basic, and the password in curl -u user:password",
		re:   regexp.MustCompile(`(?i:\bauthorization["']?\s*[:=]\s*["']?basic\s+([A-Za-z0-9+/=]{8,}))|\bcurl\b[^\n|;&]*?\s(?:-u|--user)[\s=]+["']?[^\s:"']+:([^\s"']{3,})`), group: -1,
		valid: validConnPassword, exact: true},
	{ID: "cookie", One: "cookie", Name: "Session cookie", Plural: "cookies", Prefix: "COOKIE", Default: true,
		Note: "the value of a Cookie or Set-Cookie header",
		re:   regexp.MustCompile(`(?i)\b(?:set-)?cookie["']?\s*:\s*["']?([^\r\n"'=;\s]+=[^\r\n"']{4,})`), group: 1, exact: true},
	{ID: "webhook", One: "webhook URL", Name: "Webhook URL", Plural: "webhook URLs", Prefix: "WEBHOOK", Default: true,
		Note: "Slack, Discord and Microsoft Teams incoming webhooks: the address is the secret",
		re:   regexp.MustCompile(`https://hooks\.slack\.com/(?:services|workflows|triggers)/([A-Za-z0-9/_-]{20,})|https://(?:canary\.|ptb\.)?discord(?:app)?\.com/api/webhooks/(\d+/[A-Za-z0-9_-]{20,})|https://[a-z0-9-]+\.webhook\.office\.com/([^\s"'<>]{20,})`), group: -1, exact: true},
	{ID: "signedurl", One: "signed URL", Name: "Signed URL", Plural: "signed URLs", Prefix: "SIGNATURE", Default: true,
		Note: "the signature or session token in an S3, Google Cloud or Azure SAS link; the rest of the link is left to read",
		re:   regexp.MustCompile(`(?i)[?&](?:X-Amz-Signature|X-Amz-Security-Token|X-Goog-Signature|Signature|sig)=([A-Za-z0-9%/+_.-]{16,})`), group: 1, exact: true},
	{ID: "envfile", One: ".env value", Name: "Values in a .env file", Plural: ".env values", Prefix: "ENV", Default: true,
		Note: "only in the output of a tool call that names a .env file: every value of 8 characters or more with a digit or mixed case. Numbers, true and false, plain words, links and paths are left to read",
		re:   regexp.MustCompile(`(?m)^(?:\s*\d+[\t→])?[ \t]*(?:export[ \t]+)?[A-Za-z_][A-Za-z0-9_.]*[ \t]*=[ \t]*["']?([^\s"'#]{8,})`), group: 1,
		valid: validEnvValue, exact: true, envOnly: true},
	{ID: "secret", One: "secret value", Name: "Secret in an assignment", Plural: "secret values", Prefix: "SECRET",
		Note: "off by default: noisy. The value after a name holding key, secret, token or password, such as API_KEY=..., when it has letters and digits",
		re:   regexp.MustCompile(`(?i)\b[A-Z0-9_.-]*(?:api[_-]?key|secret|token|passw(?:or)?d)[A-Z0-9_]*["']?\s*[:=]\s*["']?([A-Za-z0-9+/_.~-]{12,}={0,2})`), group: 1,
		valid: validSecret, exact: true},
	{ID: "card", One: "credit card number", Name: "Credit card number", Plural: "credit card numbers", Prefix: "CARD", Default: true,
		Note: "Visa, Mastercard, Amex, Discover, Diners, JCB; Luhn checked, so random long numbers are left alone",
		re:   regexp.MustCompile(`\b\d(?:[ -]?\d){12,18}\b`), valid: validCard, keep4: true},
	{ID: "said", One: "SA ID number", Name: "South African ID number", Plural: "SA ID numbers", Prefix: "SAID", Default: true,
		Note: "13 digits with a real birth date and a valid check digit",
		re:   regexp.MustCompile(`\b\d{13}\b`), valid: validSAID},
	{ID: "ssn", One: "US SSN", Name: "US Social Security number", Plural: "US SSNs", Prefix: "SSN", Default: true,
		Note: "123-45-6789 form, invalid area, group and serial numbers excluded",
		re:   regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`), valid: validSSN},
	{ID: "nino", One: "UK NI number", Name: "UK National Insurance number", Plural: "UK NI numbers", Prefix: "NINO", Default: true,
		Note: "AB 12 34 56 C form, prefixes that are never issued excluded",
		re:   regexp.MustCompile(`\b[A-CEGHJ-PR-TW-Z][A-CEGHJ-NPR-TW-Z] ?\d{2} ?\d{2} ?\d{2} ?[A-D]\b`), valid: validNINO},
	{ID: "iban", One: "IBAN", Name: "IBAN", Plural: "IBANs", Prefix: "IBAN", Default: true,
		Note: "mod-97 checked",
		re:   regexp.MustCompile(`\b[A-Z]{2}\d{2}(?: ?[A-Z0-9]){11,30}\b`), valid: validIBAN},
	{ID: "passport", One: "passport line", Name: "Passport (machine-readable line)", Plural: "passport lines", Prefix: "PASSPORT", Default: true,
		Note: "the second line of a passport's machine-readable zone, check digits verified",
		re:   regexp.MustCompile(`[A-Z0-9<]{9}\d[A-Z<]{3}\d{6}\d[MF<]\d{6}\d`), valid: validMRZ},
	{ID: "email", One: "email address", Name: "Email address", Plural: "email addresses", Prefix: "EMAIL",
		Note: "off by default: code, configs and git logs are full of them",
		re:   regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}\b`)},
	{ID: "phone", One: "phone number", Name: "Phone number", Plural: "phone numbers", Prefix: "PHONE",
		Note: "off by default: noisy. International +27 form and SA mobile numbers",
		re:   regexp.MustCompile(`(?:\+\d{8,15}\b|\b0[6-8]\d[ -]?\d{3}[ -]?\d{4}\b)`)},
	{ID: "bankacc", One: "bank account number", Name: "South African bank account", Plural: "bank account numbers", Prefix: "BANKACC",
		Note: "off by default: 9 to 11 digits only straight after a word like \"account\"",
		re:   regexp.MustCompile(`(?i)\bacc(?:ount)?(?:\s*(?:no\.?|number|#))?[\s:]+(\d{9,11})\b`), group: 1},
	{ID: "ipv4", One: "IP address", Name: "IPv4 address", Plural: "IP addresses", Prefix: "IP",
		Note: "off by default: logs and configs are full of them",
		re:   regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`), valid: validIPv4},
}

// Hit is one value masked for the first time in a session.
type Hit struct {
	Rule  *Rule
	Mask  string
	Where string
}

// Session holds one session's masks, so the same value always gets the same
// mask: the prompt cache and the compaction hash stay stable.
type Session struct {
	mu     sync.Mutex
	masks  map[string]string // rule|value -> mask
	counts map[string]int    // rule -> masks handed out
	seen   time.Time
}

func NewSession() *Session {
	return &Session{masks: map[string]string{}, counts: map[string]int{}}
}

// Mask replaces every match of the enabled rules in text. hits lists the
// values masked for the first time in this session; a value seen before
// gets its old mask and is not a hit.
func (s *Session) Mask(text, where string, on func(*Rule) bool) (string, []Hit, bool) {
	return s.mask(text, where, on, false)
}

// MaskEnvFile is Mask for the output of a tool call that names a .env
// file: the rules that mask a file's values by where they sit apply too.
func (s *Session) MaskEnvFile(text, where string, on func(*Rule) bool) (string, []Hit, bool) {
	return s.mask(text, where, on, true)
}

func (s *Session) mask(text, where string, on func(*Rule) bool, env bool) (string, []Hit, bool) {
	var hits []Hit
	changed := false
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = time.Now()
	for _, r := range Rules {
		if !on(r) || r.envOnly && !env {
			continue
		}
		idx := r.re.FindAllStringSubmatchIndex(text, -1)
		if len(idx) == 0 {
			continue
		}
		var b strings.Builder
		last := 0
		for _, m := range idx {
			g := r.group
			for i := 1; g < 0 && 2*i < len(m); i++ {
				if m[2*i] >= 0 {
					g = i
				}
			}
			if g < 0 {
				continue
			}
			lo, hi := m[2*g], m[2*g+1]
			if lo < 0 {
				continue
			}
			v := text[lo:hi]
			if r.valid != nil && !r.valid(v) {
				continue
			}
			key := r.ID + "|" + v
			if !r.exact {
				key = r.ID + "|" + normalise(v)
			}
			mask, ok := s.masks[key]
			if !ok {
				s.counts[r.ID]++
				mask = fmt.Sprintf("[%s-%d]", r.Prefix, s.counts[r.ID])
				if r.keep4 {
					d := digits(v)
					mask = fmt.Sprintf("[%s-%d ...%s]", r.Prefix, s.counts[r.ID], d[len(d)-4:])
				}
				s.masks[key] = mask
				hits = append(hits, Hit{Rule: r, Mask: mask, Where: where})
			}
			b.WriteString(text[last:lo])
			b.WriteString(mask)
			last = hi
			changed = true
		}
		if last > 0 {
			b.WriteString(text[last:])
			text = b.String()
		}
	}
	return text, hits, changed
}

// Seen is when the session last masked anything, for pruning idle ones.
func (s *Session) Seen() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen
}

// Summary is "2 credit card numbers and 1 SA ID number" for a set of hits.
func Summary(hits []Hit) string {
	n := map[*Rule]int{}
	var order []*Rule
	for _, h := range hits {
		if n[h.Rule] == 0 {
			order = append(order, h.Rule)
		}
		n[h.Rule]++
	}
	sort.SliceStable(order, func(i, j int) bool { return ruleIndex(order[i]) < ruleIndex(order[j]) })
	var parts []string
	for _, r := range order {
		label := r.Plural
		if n[r] == 1 {
			label = r.One
		}
		parts = append(parts, strconv.Itoa(n[r])+" "+label)
	}
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

func ruleIndex(r *Rule) int {
	for i, x := range Rules {
		if x == r {
			return i
		}
	}
	return len(Rules)
}

func normalise(v string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' {
			return -1
		}
		return r
	}, v)
}

func digits(v string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, v)
}

func luhn(d string) bool {
	sum, alt := 0, false
	for i := len(d) - 1; i >= 0; i-- {
		n := int(d[i] - '0')
		if alt {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
		alt = !alt
	}
	return sum%10 == 0
}

func validCard(v string) bool {
	d := digits(v)
	if len(d) < 13 || len(d) > 19 || !luhn(d) {
		return false
	}
	if len(d) == 13 && validSAID(d) {
		return false // the SA ID rule's, which says what it is
	}
	p2, _ := strconv.Atoi(d[:2])
	p4, _ := strconv.Atoi(d[:4])
	switch {
	case d[0] == '4':
		return true
	case p2 >= 51 && p2 <= 55, p4 >= 2221 && p4 <= 2720: // Mastercard
		return len(d) == 16
	case p2 == 34 || p2 == 37: // Amex
		return len(d) == 15
	case p4 == 6011 || p2 == 65: // Discover
		return true
	case p2 == 36 || p2 == 38 || (p2 == 30 && d[2] >= '0' && d[2] <= '5'): // Diners
		return true
	case p2 == 35: // JCB
		return true
	}
	return false
}

// mixed reports whether v has a digit, and a letter of each case.
func mixed(v string) (digit, upper, lower bool) {
	for _, c := range v {
		switch {
		case c >= '0' && c <= '9':
			digit = true
		case c >= 'A' && c <= 'Z':
			upper = true
		case c >= 'a' && c <= 'z':
			lower = true
		}
	}
	return
}

// validAPIKey leaves alone what only looks like an sk- key: a dashed name
// such as sk-learn-something-or-other has no digit or no mixed case. The
// other prefixes are distinctive enough as they are.
func validAPIKey(v string) bool {
	digit, upper, lower := mixed(v)
	if strings.Contains(v, ".") && !strings.HasPrefix(v, "SG.") {
		return digit // a bot token; a dotted name in code has none
	}
	if !strings.HasPrefix(v, "sk-") {
		return true
	}
	return digit && (upper && lower || len(v) >= 40)
}

// validConnPassword leaves a placeholder alone: $DB_PASSWORD, ${pw},
// <password>, %s and **** are not passwords.
func validConnPassword(v string) bool {
	return !strings.ContainsAny(v[:1], "$<{%*[")
}

// validToken wants a digit in what follows Bearer: "Bearer token_goes_here"
// in a document is not a token. A JWT always has one.
func validToken(v string) bool {
	digit, _, _ := mixed(v)
	return digit && validConnPassword(v)
}

// validEnvValue is a .env value worth masking: one with a digit or mixed
// case that is not a number, a link, a path, a host name or already a mask.
func validEnvValue(v string) bool {
	digit, upper, lower := mixed(v)
	if !digit && !(upper && lower) || strings.ContainsAny(v, "[]") || strings.ContainsAny(v[:1], "$<{%*/~.") {
		return false
	}
	if _, err := strconv.ParseFloat(v, 64); err == nil {
		return false
	}
	if strings.Contains(v, "://") || !upper && strings.Contains(v, ".") {
		return false // a link (its password is the connection string rule's) or a host name
	}
	return true
}

// validSecret wants letters and digits: a name, a path or a call on the
// right of the equals sign is code, not a secret.
func validSecret(v string) bool {
	digit, upper, lower := mixed(v)
	return digit && (upper || lower) && !strings.Contains(v, "..") && !strings.HasPrefix(v, "process.") && !strings.HasPrefix(v, "os.")
}

func validSAID(v string) bool {
	if len(v) != 13 || !luhn(v) {
		return false
	}
	if _, err := time.Parse("060102", v[:6]); err != nil {
		return false
	}
	return (v[10] == '0' || v[10] == '1') && (v[11] == '8' || v[11] == '9')
}

func validSSN(v string) bool {
	area, group, serial := v[:3], v[4:6], v[7:]
	return area != "000" && area != "666" && area[0] != '9' && group != "00" && serial != "0000"
}

func validNINO(v string) bool {
	switch v[:2] {
	case "BG", "GB", "NK", "KN", "TN", "NT", "ZZ":
		return false
	}
	return true
}

func validIBAN(v string) bool {
	s := normalise(v)
	if len(s) < 15 || len(s) > 34 {
		return false
	}
	s = s[4:] + s[:4]
	rem := 0
	for _, c := range s {
		var n int
		switch {
		case c >= '0' && c <= '9':
			n = int(c - '0')
			rem = (rem*10 + n) % 97
		case c >= 'A' && c <= 'Z':
			n = int(c-'A') + 10
			rem = (rem*100 + n) % 97
		default:
			return false
		}
	}
	return rem == 1
}

// mrzCheck is the ICAO 9303 check digit: weights 7, 3, 1; < is 0, A is 10.
func mrzCheck(s string) int {
	w := []int{7, 3, 1}
	sum := 0
	for i, c := range s {
		n := 0
		switch {
		case c >= '0' && c <= '9':
			n = int(c - '0')
		case c >= 'A' && c <= 'Z':
			n = int(c-'A') + 10
		}
		sum += n * w[i%3]
	}
	return sum % 10
}

func validMRZ(v string) bool {
	// number(9) check(1) nationality(3) birth(6) check(1) sex(1) expiry(6) check(1)
	return mrzCheck(v[0:9]) == int(v[9]-'0') &&
		mrzCheck(v[13:19]) == int(v[19]-'0') &&
		mrzCheck(v[21:27]) == int(v[27]-'0')
}

func validIPv4(v string) bool {
	for _, p := range strings.Split(v, ".") {
		n, err := strconv.Atoi(p)
		if err != nil || n > 255 || (len(p) > 1 && p[0] == '0') {
			return false
		}
	}
	return true
}
