# Automask

User guide: [../automask.md](../automask.md). Status: built 2026-10-04 (`internal/automask`, `internal/router/automask.go`, the dashboard's Automask section). Off by default. Asked for 2026-10-03.

## What it does

The gateway looks for PII in every request it forwards. When a rule matches it
does **not** reject the request: it replaces the match with a mask, sends the
masked request on, logs the hit and shows a warning in that session.

- **Default: off.** A dashboard switch turns it on; each rule has its own switch.
- **Mask, never reject.** A refused request breaks the turn; a masked one does not.
- **Log** each hit to the gateway log: session, rule, where (user text, tool
  result, system), and the masked form only. The original value is never
  written anywhere.
- **Session warning** through the existing prompt-notice line, once per rule
  per prompt, e.g. `Claude Burst, automask: masked 2 credit card numbers and 1 SA ID number in this request`.
  The same event goes to `notices.json` (kind `automask`) for the panel.

## Masking rules

- Deterministic mask per value within a session: `[CARD-1]`, `[CARD-2]`,
  `[SAID-1]`. The same value gets the same mask every request, so the
  compaction prefix hash and Anthropic's prompt cache stay stable.
- Keep the last four of a card (`[CARD-1 ...4242]`) so Claude can still tell
  cards apart in its answer.
- Mask in every message, including earlier turns and tool results, not just
  the new prompt: Claude Code resends the whole conversation each request.
- Never touch `cache_control`, thinking blocks or tool_use ids.
- Validators run after the regex where one exists (Luhn, ID checksums), so
  random 16-digit numbers in logs and hashes are not masked.

## Default rules

| Rule | Regex (sketch) | Validator | Default |
|---|---|---|---|
| Private key block | `-----BEGIN ... PRIVATE KEY-----` to `-----END ... PRIVATE KEY-----` | none | on |
| API key | known prefixes: `sk-`, `AKIA`/`ASIA`, `ghp_`/`github_pat_`, `glpat-`, `AIza`, `xox?-`, `sk_live_`/`rk_live_`, `npm_`, `hf_`, `SG.` | an `sk-` match needs a digit and mixed case (or 40 characters), so dashed names are left alone | on |
| Connection string password | `scheme://user:PASSWORD@host`; `;Password=`, `;Pwd=`, `;AccountKey=`, `;SharedAccessKey=` | not a placeholder (`$VAR`, `<password>`, `%s`, `****`) | on |
| Bearer token or JWT | `eyJ...` three-part token; what follows `Bearer` or `Authorization:` | a digit, not a placeholder | on |
| Basic auth credentials | `Authorization: Basic ...`; `curl -u user:PASSWORD` | not a placeholder | on |
| Session cookie | value of `Cookie:` / `Set-Cookie:` holding `name=value` | none | on |
| Webhook URL | Slack, Discord, Teams incoming webhook paths | none | on |
| Signed URL | `X-Amz-Signature=`, `X-Amz-Security-Token=`, `X-Goog-Signature=`, `Signature=`, `sig=` | 16 characters or more | on |
| Values in a .env file | `NAME=value` lines, only in the output of a tool call whose input names a `.env` file | 8 characters or more with a digit or mixed case; not a number, link, path or host name | on |
| Your own words | the user's list (`automask.words`), whole words, any case; runs last | 3 to 100 characters, 500 at most | on |
| Secret in an assignment | value after a name holding key, secret, token or password | letters and digits, 12 characters or more | off (noisy) |
| Credit card (Visa, Mastercard, Amex, Discover, Diners, JCB) | `\b(?:\d[ -]?){13,19}\b` | Luhn + known IIN prefix (4, 51-55, 2221-2720, 34/37, 6011/65, 36/38, 35) | on |
| South African ID number | `\b\d{2}(0[1-9]\|1[0-2])(0[1-9]\|[12]\d\|3[01])\d{4}[01][89]\d\b` | Luhn on all 13 digits, valid date | on |
| US Social Security number | `\b(?!000\|666\|9\d\d)\d{3}-(?!00)\d{2}-(?!0000)\d{4}\b` | area/group/serial not zero | on |
| UK National Insurance number | `\b[A-CEGHJ-PR-TW-Z]{2}\s?\d{2}\s?\d{2}\s?\d{2}\s?[A-D]\b` | excluded prefixes (BG, GB, NK, KN, TN, NT, ZZ) | on |
| IBAN | `\b[A-Z]{2}\d{2}[A-Z0-9]{11,30}\b` | mod-97 check | on |
| Passport number (ICAO MRZ line) | `\b[A-Z0-9<]{9}\d[A-Z]{3}\d{6}\d[MF<]\d{6}\d` | MRZ check digits | on |
| Email address | `\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b` | none | off (code is full of them) |
| Phone number (E.164 and SA local) | `\+\d{8,15}\b`, `\b0[6-8]\d[ -]?\d{3}[ -]?\d{4}\b` | length | off (noisy) |
| South African bank account | `\b\d{9,11}\b` near "account"/"acc" | context word | off (noisy) |
| IPv4 address | standard dotted quad | octet range | off |

Sources for the list: Microsoft Presidio's predefined recognizers and Google
Cloud DLP infoTypes (credit card, national IDs, IBAN, email, phone), plus the
SA ID number because the corporate users are in South Africa.

Secrets (API keys, tokens) are a separate feature; Burst's existing key
redaction covers logs only.

## Open questions

- Does the reply get unmasked for display? Proposal: no. Claude only ever
  sees masks, and Burst never stores the originals.
- Custom rules: a `rules` list in config.json (name, regex, validator, mask
  prefix), editable on the dashboard. Not built: `automask.rules` holds only on/off overrides of the built-in rules.
- Cost: built as proposed. Each message is cached by its hash per session, so only new messages are scanned.
