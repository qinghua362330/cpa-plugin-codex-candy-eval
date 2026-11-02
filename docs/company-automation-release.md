# Automatic fingerprint inspection: 0.4.4-company.1

## Delivery status

- Upstream source: `haowang02/cpa-plugin-codex-candy-eval`, commit `179d6dae48a0f0a735637ea2f373345b91caaca8`.
- Locally built for Linux/amd64 using Go 1.26.2 and Zig 0.15.2, glibc target 2.17.
- Artifact: `cpa-codex-candy-eval-v0.4.4-company.1.so`.
- SHA256: `e57b9cbc90455f1b637f0edb7cdb2642546a04ba347bf0c2776dfb49114b2485`.
- Not deployed. No production credentials, timers, or CPA configuration were changed.
- Both scheduled inspection and automatic disabling default to off. No default model is silently selected.

## Verification

Passed `go vet ./...`, `go test -race ./... -count=1`, `node scripts/test-ui.cjs`, `git diff --check`, Linux shared-library build, and two review passes. Browser checks covered desktop and mobile layouts with mocked responses.

Tests cover the real timer firing and rearming, serialized execution, cancellation, reload persistence, complete/incomplete verdicts, model aliases, stale credential identity and revision, storage failures, narrow status PATCH, response verification, secret redaction, loopback restrictions, redirect rejection, and disabled proxy use.

No live model evaluation, live credential disabling, or Linux host loading smoke test has been run with this custom build. Local integration tests use mock host callbacks and loopback HTTP test servers. Stored management configuration readiness does not prove a live management connection.

## Installation boundary

The original CPA process does not need a rebuild for this feature. Before installation, back up its plugin configuration, existing plugin library, and plugin state. Install the versioned library and set the plugin's exact version pin using CPA's supported plugin management flow. This is a prerelease version; do not rely on automatic highest-version selection against the stable `0.4.4` library. Preserve the previous library for rollback.

After loading, verify the active plugin version is `0.4.4-company.1`, its management page exposes automatic inspection, and the default policy is off. Use one selected credential in observe-only mode before enabling a recurring policy. Do not run the upstream deploy script blindly: it restarts CPA.

Automatic disabling additionally requires explicit local management configuration in the plugin page. Use the CPA listener inside its own container, not a public reverse-proxy URL or host-mapped port. The secret is stored separately at `plugins/cpa-codex-candy-eval-state.json.management.json` with mode `0600`, and never returned to the page. Only the original normal login flow stores its existing management login key; this new connection form does not add browser secret storage.

## Operational limits

Fingerprint attribution is probabilistic. A complete mismatch is acted on without an additional probability threshold or repeat-confirmation policy. Disabling affects the whole credential, including other models. There is no automatic re-enable policy.

The plugin never writes a complete credential file. It calls CPA's status PATCH after checking the original test snapshot. CPA v8.0.51 applies that status using its mutation gate. An external replacement in the final snapshot-to-server-lookup interval cannot be excluded by this API. Avoid concurrent manual replacement of tested files. Runtime and disk state are checked after the PATCH; uncertain outcomes require operator review.

Rolling back the library does not undo disabled credentials. Stop scheduling first, restore the previous exact version pin/library, retain the audit file, then review disabled credentials individually. Protect backups because the separate management configuration contains a secret.
