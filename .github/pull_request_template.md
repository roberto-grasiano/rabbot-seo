<!-- Thanks for contributing! A short description of the change and why it's needed. -->

## Checklist

- [ ] The failing test came first (TDD) and the new tests fail without the change
- [ ] `make all` is green locally (tidy, vet, lint, race tests, build)
- [ ] No cgo dependencies added — the static pure-Go build is non-negotiable
- [ ] No edits to applied migrations (`internal/store/migrations/`) — schema changes are a **new** `NNNN_name.sql`
- [ ] No secrets (webhook URLs, tokens, SMTP credentials) in code, tests, fixtures, or logs
