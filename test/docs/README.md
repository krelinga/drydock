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
