#!/bin/sh
# Adds one entry to Claude Burst's audit trail (the Audit tab): what a script
# did, and how it ended. The dashboard records its own actions; before this
# the scripts recorded nothing, so a rollback, a repair or the watchdog
# killing a hung gateway left no trace of who did what.
#
#   audit-add.sh KIND SEVERITY TITLE [DETAIL]      severity: info ok warn error
#
# Written against python3 and nothing else on purpose: the scripts that call
# it are the ones run when the gateway binary is missing or broken. It never
# fails its caller: any error here exits 0 having written nothing.
FILE="${CLAUDE_BURST_AUDIT_FILE:-$HOME/.config/claude-burst/audit.jsonl}"
[ $# -ge 3 ] || { echo "usage: audit-add.sh KIND SEVERITY TITLE [DETAIL]" >&2; exit 0; }
/usr/bin/python3 - "$FILE" "$1" "$2" "$3" "${4:-}" <<'PY' 2>/dev/null || true
import datetime, json, os, sys, time

path, kind, severity, title, detail = sys.argv[1:6]
if severity not in ("info", "ok", "warn", "error"):
    severity = "info"
now = time.time_ns()
at = datetime.datetime.fromtimestamp(now / 1e9).astimezone()
ev = {"id": "%d-%d" % (now, os.getpid()), "kind": kind, "severity": severity, "title": title[:300]}
if detail:
    ev["detail"] = detail[:1000]
ev.update({"at": at.isoformat(), "ts": now // 10**9, "audit_only": True})
# The folder is never created here: after an uninstall --purge there is no
# Claude Burst left to keep an audit for, and writing one would put its
# folder back.
if not os.path.isdir(os.path.dirname(path)):
    sys.exit(0)
# Same limit as the gateway's writer (notice.AuditMax): past it the file
# moves to audit.jsonl.1 and a new one starts.
try:
    if os.path.getsize(path) > 2 << 20:
        os.replace(path, path + ".1")
except OSError:
    pass
fd = os.open(path, os.O_CREAT | os.O_APPEND | os.O_WRONLY, 0o600)
with os.fdopen(fd, "a") as f:
    f.write(json.dumps(ev, ensure_ascii=False) + "\n")
PY
exit 0
