# Upgrading: instance-bound verification

Verification tokens are now derived from a per-instance secret key stored at
`<data_dir>/instance.key` (created automatically, `0600`). This makes a verified
site non-spoofable — a token placed for one install cannot verify the same domain
in another, and editing `config.yaml` cannot fake a verified state.

**What you need to do after upgrading:**

- Sites you verified before this change revert to **throttled** on the first
  re-check (they still run — just at the conservative pace). To restore full
  speed, run `rabbot verify <site>` once: it prints the new token to place,
  you place it, and re-running confirms it.
- **Back up your data dir.** `instance.key` lives there with your database. If you
  move the install to another machine, move the whole data dir; if you lose the
  key, every site reverts to throttled until its (new) token is re-placed.
- `rabbot verify --token …` is gone — the token is always derived from your
  instance key, so there is nothing to pass.
