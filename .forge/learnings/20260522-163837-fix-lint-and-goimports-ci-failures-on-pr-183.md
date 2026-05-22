# Learnings - 2026-05-22 16:38

- Never claim CI status without actually running `gh pr checks <number> --watch` and waiting for completion. Local `go vet` and `gofmt` are necessary but not sufficient — the CI pipeline runs golangci-lint (errcheck, staticcheck) and goimports which catch issues `go vet` misses.
- When staticcheck reports SA4008 (loop condition never changes) or SA4004 (unconditionally terminated loop), always read the surrounding code to understand whether the loop is genuinely broken or was always meant to run once. In this case, `startSSERelay` is async (spawns goroutine), so the retry was delegated to recursive `handleSSEDisconnect` calls — the loop was dead code.
- The `go build` command leaves binaries in the working directory. Always check `git diff` before committing to avoid accidentally staging built binaries. Better: clean up with `rm -f` before `git add -A`, or use a .gitignore entry.
