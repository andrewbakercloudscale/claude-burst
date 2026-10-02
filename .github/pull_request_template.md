**What this changes, and why**

<!-- One or two sentences. Link the issue if there is one. -->

**How it was tested**

<!-- Commands run and what they showed. Never run root scripts in CI or tests. -->

- [ ] `go test ./... -race` and `go vet ./...` pass
- [ ] `python3 scripts/ci/check-doc-links.py` passes (if docs changed)
- [ ] If it changes what Burst installs or touches on a Mac: ROLLBACK.md and the README's "What it changes on your Mac" box are updated
- [ ] No em dashes or en dashes anywhere in the change
