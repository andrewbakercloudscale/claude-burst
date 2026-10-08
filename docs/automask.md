# Automask

Automask replaces secrets and personal data with a mask in every request before it leaves your Mac. Claude sees `[APIKEY-1]`, not the key. It is off by default.

![Automask: the master switch and one switch per rule, with how many values each rule masked](screenshots/automask.png)

## Why

Claude Code sends the whole conversation with every request: your prompts, every file it read and every command's output. A `.env` file read once, a `curl -v` with a bearer token, or a row of customer data in a query result is then sent again on every turn until the conversation is compacted. Automask takes the value out on the way.

## Switching it on

Dashboard, **Claude** tab, **Context** menu, **Automask**: tick **Mask personal data and secrets**, pick the rules, **Save**. It applies from the next request; nothing restarts.

In `config.json`:

```json
"automask": {
  "enabled": true,
  "rules": { "email": true, "envfile": false },
  "words": ["bluebird", "acme-prod.internal"]
}
```

`rules` only needs the rules you changed from their default. `words` is your own list, below.

## What it masks

Secrets, on once Automask is on:

| Rule | Config key | What is masked | Example of what Claude sees |
|---|---|---|---|
| Private key block | `privatekey` | A PEM block from BEGIN to END PRIVATE KEY (RSA, EC, OpenSSH, PGP), also inside a Google service account file | `[PRIVATEKEY-1]` |
| API key | `apikey` | Keys with a known shape: Anthropic, OpenAI, AWS, GitHub, GitLab, Google, Slack, Stripe, npm, PyPI, Hugging Face, SendGrid, Twilio, Mailgun, DigitalOcean, Shopify, Databricks, Discord and Telegram bots | `ANTHROPIC_API_KEY=[APIKEY-1]` |
| Connection string password | `connstr` | The password in `scheme://user:password@host`, and `Password=`, `Pwd=`, `AccountKey=`, `SharedAccessKey=` after a semicolon | `postgres://app:[CONNSTR-1]@db:5432/shop` |
| Bearer token or JWT | `token` | A JSON Web Token anywhere, and what follows `Bearer` or an `Authorization` header | `Authorization: Bearer [TOKEN-1]` |
| Basic auth credentials | `basicauth` | What follows `Authorization: Basic`, and the password in `curl -u user:password` | `curl -u deploy:[BASICAUTH-1]` |
| Session cookie | `cookie` | The value of a `Cookie` or `Set-Cookie` header | `Set-Cookie: [COOKIE-1]` |
| Webhook URL | `webhook` | Slack, Discord and Microsoft Teams incoming webhooks | `https://hooks.slack.com/services/[WEBHOOK-1]` |
| Signed URL | `signedurl` | The signature or session token in an S3, Google Cloud or Azure SAS link | `...&X-Amz-Signature=[SIGNATURE-1]` |
| Values in a .env file | `envfile` | In the output of a tool call that names a `.env` file: every value of 8 characters or more with a digit or mixed case | `DATADOG_KEY=[ENV-1]` |

Personal data, on once Automask is on:

| Rule | Config key | Checked by |
|---|---|---|
| Credit card number | `card` | Luhn and a known card prefix; the last four digits stay: `[CARD-1 ...4242]` |
| South African ID number | `said` | A real birth date and the check digit |
| US Social Security number | `ssn` | Invalid area, group and serial numbers excluded |
| UK National Insurance number | `nino` | Prefixes that are never issued excluded |
| IBAN | `iban` | The mod-97 check |
| Passport machine-readable line | `passport` | The check digits |

## Your own words

Under the rules is a box for your own list, one to a line: project code names, customer names, internal host names, anything no pattern could know. Each is masked as `[WORD-1]`, `[WORD-2]` wherever it appears.

- **Whole words, whatever their case.** `bluebird` masks `Bluebird` and `BLUEBIRD`, not `bluebirds` or `bluebird_v2`. Add those forms as lines of their own if you want them gone too.
- **A line can hold dots, dashes and spaces**: `acme-prod.internal`, `Acme Holdings`. The longest line wins where two overlap.
- **3 characters or more, up to 500 lines.** Shorter lines are dropped when you save.
- **Think before adding a word your code uses.** If `bluebird` is also a folder name, Claude sees `src/[WORD-1]/main.go` and cannot open that path.
- The list is kept in `config.json` on this Mac (`automask.words`) and is switched by the **Your own words** rule (`words`).

## Last 50 masks

The section ends with the last 50 masks: when, which repository, which rule, where it was found (your prompt, tool output, Claude's reply, the system prompt) and the mask. A value is listed the first time it is masked in a session, and the value itself is never kept. The list starts again when the gateway restarts; the gateway log holds the same lines for longer.

There to switch on (noisy in code, so off):

| Rule | Config key | What is masked |
|---|---|---|
| Secret in an assignment | `secret` | The value after a name holding key, secret, token or password, such as `API_KEY=...`, when it has letters and digits |
| Email address | `email` | Any email address |
| Phone number | `phone` | International `+` numbers and South African mobile numbers |
| South African bank account | `bankacc` | 9 to 11 digits straight after a word like "account" |
| IPv4 address | `ipv4` | Any valid address |

## How it behaves

- **It never refuses a request.** A refused request breaks the turn; a masked one does not.
- **Only the secret part goes.** A connection string keeps its host, port and database; a signed link keeps its path; a `.env` file keeps its names, ports, links and paths.
- **The same value gets the same mask for the whole session.** Claude can tell two keys apart (`[APIKEY-1]`, `[APIKEY-2]`), and Anthropic's prompt cache and Burst's compaction keep working.
- **You are told once per value.** A line under the prompt, a pop-up, and a line in the gateway log with the session, the rule and the mask. The value itself is never written anywhere.
- **Everything text is looked at**: the system prompt, your prompts, tool output and earlier turns. Thinking blocks, pictures and the tool calls Claude writes are left as they are.
- **The dashboard counts** how many values each rule masked since the gateway started. The [Context inspector](dashboard.md#context-inspector) flags an item holding something a rule would mask.

## What to expect when a key is masked

A masked key is a key Claude cannot use. If Claude reads `.env` and then writes `curl -H "Authorization: Bearer [APIKEY-1]"`, the call fails. That is the point: the key did not leave the Mac. Have the command read the key when it runs:

```bash
curl -H "Authorization: Bearer $ANTHROPIC_API_KEY" ...
set -a; . ./.env; set +a; ./deploy.sh
```

## Limits

- **It is pattern matching, not understanding.** A key with no recognisable shape (Datadog, Cloudflare and Azure keys are plain hex or base64) is only caught in a `.env` file or by the `secret` rule. A password in a sentence is not caught at all.
- **Names, addresses and other free text** are not masked, unless they are on your own word list.
- **Tool calls are not masked.** If Claude itself types a secret into a command, that text is sent as written.
- **Requests only.** Claude's answers are not changed on the way back.
- **Claude Code only.** Codex requests pass through unchanged.
- **Switching a rule on rewrites history once.** A session that already holds a matching value pays one cache rewrite the first time it is masked.
- **The local transcript is not changed.** Claude Code's own files under `~/.claude` still hold what you typed and what tools returned.

The design note, with the reasoning behind each rule, is [design/automask.md](design/automask.md).
