# Company upstream sync and releases

Owned fork: `qinghua362330/cpa-plugin-codex-candy-eval`, branch `main`.
Upstream: `haowang02/cpa-plugin-codex-candy-eval`, branch `main`.

## Triggers and safety

- `company-upstream-sync.yml`: every six hours at 00:17, 06:17, 12:17,
  18:17 UTC (`17 */6 * * *`), or manual dispatch on `main`. GitHub schedules
  can be delayed; they are not an exact wall-clock service guarantee.
- `release.yml`: a user pushes `v*-company.*`, manual dispatch with an existing
  company tag, or a direct reusable-workflow call from a successful sync.
  Ordinary upstream `v0.4.4` tags do not publish releases.
- `company-ci.yml`: main pushes, pull requests targeting main, or manual dispatch.

Sync fetches upstream main without importing upstream tags. In a detached local
commit it first normalizes only the exact `pluginVersion` scalar to the merge-base
source value. All other company source changes remain untouched. It then uses an
ordinary three-way `--no-ff --no-commit` merge, preserving company history and upstream
features. No force push, reset-to-upstream, broad ours/theirs resolution, or
rebase is used. Any remaining merge conflict aborts the
merge, restores the original checkout, and creates or updates the bot-owned **Company upstream sync blocked**
issue. Remote main and tags remain unchanged. Validation and push failures also
report there. Enable Issues on the fork so these reports can be created.

After a clean merge only the `pluginVersion` declaration in
`internal/plugin/plugin.go` is updated by the sync script, to
`<upstream-version>-company.<UTC YYYYMMDDHHMMSS>.<run-id>.<attempt>`.
Version extraction must be unambiguous. The timestamp and run identity distinguish
future builds from both upstream and the initial `0.4.4-company.1` build.
Routine upstream version bumps merge automatically because of scalar normalization.
Same-line implementation changes still conflict and require manual review; source
structure changes or ambiguous version declarations also fail closed.

The merged tree must pass `go vet ./...`, `go test -race ./... -count=1`,
`node scripts/test-ui.cjs`, and diff whitespace checks before the merge is committed.
Main and its annotated tag are pushed together with `git push --atomic`, without
force. A concurrent main advance or tag collision rejects both updates. No new
upstream commits means no merge, version bump, tag, or release.

The release workflow retests the exact tag and checks it is an ancestor of origin
main and exactly matches the source version. It builds Linux amd64 with the same
manylinux2014/glibc 2.17 baseline used upstream. Before packaging, the builder runs
`python3 scripts/smoke-shared-library.py <library> <version>` to load the real ABI,
assert exact metadata version and default-off automation in isolated temporary
state, and quiesce the plugin. The parent-owned smoke script must be committed
alongside the initial release. Assets are:

- `cpa-codex-candy-eval_<version>_linux_amd64.tar.gz`, containing
  `cpa-codex-candy-eval-v<version>.so` at the root.
- `cpa-codex-candy-eval_<version>_linux_amd64.zip`, containing the upstream
  store-compatible `cpa-codex-candy-eval.so` at the root.
- The versioned `.so` itself and `checksums.txt` covering all three assets.

Releases are explicitly **not prereleases** and are marked **latest**, despite
the company SemVer suffix, so the fork's latest-release API serves the store.
Existing releases/assets are never overwritten. Releasing an older tag manually
will also make it latest: only dispatch the intended current company version.
If a release build fails after sync pushes, main/tag remain available; rerun the
failed job or manually dispatch that same tag. Do not create a second merge just
to retry a build. Inspect any partially created release before retrying publish;
the workflow deliberately does not clobber existing assets.

## Token and repository settings

Only the built-in `GITHUB_TOKEN` is required. Sync needs `contents: write` and
`issues: write`; the reusable release caller and publish job need `contents: write`.
Testing/build jobs have read-only contents permission. No PAT, SSH secret,
production host, deployment job, remote restart, or production credentials are used.

The sync calls the reusable workflow directly, because a `GITHUB_TOKEN` push does
not ordinarily trigger another push workflow. See GitHub's
[trigger rules](https://docs.github.com/en/actions/how-tos/write-workflows/choose-when-workflows-run/trigger-a-workflow)
and [reusable workflow documentation](https://docs.github.com/en/actions/how-tos/reuse-automations/reuse-workflows).
Branch/tag rules must permit the bot's atomic update; otherwise the sync fails
closed. Upstream changes to workflow files may also be rejected by GitHub token
policy and require a maintainer-reviewed merge. Do not add a privileged token or
weaken branch protection automatically. Sync issues remain open for manual review
and closure after resolution.

## First release: parent/operator commands

First commit the integrated company changes and these files on main and push main
to the owned fork. Do not tag an uncommitted working tree. Confirm `pluginVersion`
is exactly `0.4.4-company.1`; scripts do not edit application files locally.
The following commands are for the parent/operator, not already executed:

```sh
cd /Users/lx/Documents/Codex/2026-11-02/new-chat/cpa-plugin-codex-candy-eval
repo=qinghua362330/cpa-plugin-codex-candy-eval
git remote -v
gh api -X PATCH "repos/$repo" -F has_issues=true
gh api -X PUT "repos/$repo/actions/permissions" -F enabled=true
gh api -X PUT "repos/$repo/actions/workflows/company-upstream-sync.yml/enable"
gh api -X PUT "repos/$repo/actions/workflows/company-ci.yml/enable"
gh api -X PUT "repos/$repo/actions/workflows/release.yml/enable"
git tag -a v0.4.4-company.1 -m 'Company release 0.4.4-company.1'
git push origin refs/tags/v0.4.4-company.1
gh run list --repo "$repo" --workflow release.yml --limit 5
```

The tag push must use the operator's normal authenticated Git credentials, not a
workflow's `GITHUB_TOKEN`. If the tag already exists, do not recreate or force it;
use the explicit recovery trigger instead:

```sh
gh workflow run release.yml --repo "$repo" --ref main -f tag=v0.4.4-company.1
gh workflow run company-upstream-sync.yml --repo "$repo" --ref main
gh api "repos/$repo/actions/workflows/company-upstream-sync.yml" --jq .state
gh api "repos/$repo/releases/latest" --jq '{tag_name, prerelease, assets: [.assets[].name]}'
```

Only run the recovery release command when the tag has no completed release.
Fork schedules need explicit enablement after these workflows exist on main.
GitHub may disable scheduled workflows after extended repository inactivity;
check workflow state periodically. Repository or organization policy can still
restrict Actions or write access; inspect run errors rather than assuming local
validation proves online authorization.

## Local validation

```sh
python3 scripts/company-sync-test.py
bash -n scripts/company-build.sh
shellcheck scripts/company-build.sh
actionlint .github/workflows/*.yml
git diff --check
```

The offline tests use disposable repositories to verify version parsing,
clean merges preserving custom commits, no-op sync, full prepare/publish with an
upstream version bump, real implementation conflict abort, and atomic rejection
of a concurrent main update. They do not
commit or push the working repository. Actual Linux packaging requires Docker
and is exercised on the GitHub Ubuntu runner; publication requires a live run.
