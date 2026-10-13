# test/docs — checks on the operator documents

- `versions_test.go`: every `vX.Y.Z` in the README and `docs/deploy/` is a release (a
  `CHANGELOG.md` heading) or the next one, computed by release-please's rule from the commits
  since the manifest last moved (plus `DRYDOCK_PR_TITLE` on a PR). So a doc pinned to a patch that
  a `feat:` turns into a minor fails that PR (a doc once pinned a release release-please never
  cut). Skips in a shallow clone unless `DRYDOCK_REQUIRE_RELEASE_HISTORY` is set, which CI does.
- `playwright_test.go`: CI's Playwright image tag matches `web/package-lock.json`'s
  `@playwright/test`, and the image is pinned by digest.
- `schema_test.go`: design §4 names every table in `internal/store`'s golden schema, and its table
  count is the golden's.
- `ritual_probe_test.go`: the VS Code connect ritual's probe packs reproducibly
  (`test/rituals/vscode-probe/pack.py`, run twice on a copy of the probe whose mtimes move
  between), and to the expected digest `test/rituals/vscode-connect.md` records, so a run's
  recorded digest can be audited against the commit. Skips without `python3`, unless
  `DRYDOCK_REQUIRE_PYTHON` is set, which CI (`.github/actions/go-suite`) and the stress job do.
