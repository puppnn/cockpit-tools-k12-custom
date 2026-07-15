# Downstream Customization Maintenance

This checkout carries a downstream customization on
`feature/api-key-routing-usage`. Keep this branch rebased on the official
`origin/main` branch instead of installing the official in-app update.

## What This Branch Adds

- Per-client-Key account-pool scopes for Codex API Service.
- Sidecar enforcement of a Key's selected account IDs.
- Per-Key session-affinity namespaces, preventing account selection from
  leaking between client Keys.
- Account-pool selection that includes newly authorized valid OAuth accounts.
- Compact Token display for client Keys, such as `82.1M` instead of
  `82100000 Tokens`.
- API compatibility URL normalization that avoids duplicate `/v1` path
  segments.

The current custom release version is `1.1.7`. The installed application is:

```text
C:\Users\admin\AppData\Local\Cockpit Tools\cockpit-tools.exe
```

## Upstream Sync Procedure

Do not click the application's official updater. It replaces the custom
binary; the stored settings persist, but the custom behavior does not.

Before starting, check that the working tree is clean:

```powershell
git status --short
git fetch origin
git switch feature/api-key-routing-usage
git branch backup/pre-upstream-sync-$(Get-Date -Format yyyyMMdd-HHmmss)
git rebase origin/main
```

When resolving conflicts:

- Preserve the official upstream behavior and release notes.
- Preserve every item in "What This Branch Adds".
- Keep version metadata aligned across `package.json`, `package-lock.json`,
  `src-tauri/Cargo.toml`, `src-tauri/tauri.conf.json`, and `Cargo.lock`.
- Set a custom patch version newer than the currently installed or official
  release before building.

Run the following verification after the rebase:

```powershell
npm run sync-version
npm run test:codex-api-key-scope
node --test src/utils/codexApiServiceCompatibility.test.ts
npm run typecheck

Set-Location sidecars\cockpit-cliproxy
& 'C:\Program Files\Go\bin\go.exe' test . -count=1

Set-Location cdk\CLIProxyAPI
& 'C:\Program Files\Go\bin\go.exe' test ./sdk/cliproxy/auth ./sdk/cliproxy/executor -count=1
```

Build and install the custom package:

```powershell
Set-Location C:\Data\Other\cockpit-tools
npm run tauri -- build
```

The build can report a nonzero exit code only because the release signing
private key is unavailable. The local NSIS package is still valid when it is
created under `target\release\bundle\nsis`.

Before stopping or replacing `cockpit-tools.exe`, start a detached hidden
watchdog. The current Codex session can depend on this process as its API
proxy. The watchdog must relaunch the executable above if it is absent.

After installing the custom NSIS package, verify:

```powershell
(Get-Item 'C:\Users\admin\AppData\Local\Cockpit Tools\cockpit-tools.exe').VersionInfo
Get-Process cockpit-tools, cockpit-cliproxy
```

Then inspect the desktop UI:

- The service status is `运行中`.
- `模型与能力` includes `gpt-5.6-sol`.
- Each client Key displays compact Token totals.
- Scoped Keys show their selected account-pool count.

Push a rebased branch with:

```powershell
git push --force-with-lease fork feature/api-key-routing-usage
```

Do not push to `origin/main`. The `fork` remote is the personal fork and
`origin` is the official repository in this checkout.

## New-Agent Prompt

In a new Codex task, use this prompt:

```text
Read docs/downstream-customization-maintenance.md. Sync
feature/api-key-routing-usage with origin/main without dropping its per-Key
account-pool routing, sidecar enforcement, session-affinity isolation, URL
compatibility, or compact Token display. Run the documented tests, build a
new custom NSIS installer, install it with a watchdog because Cockpit is the
active API proxy, verify the desktop UI, and push the rebased branch to fork
with --force-with-lease. Do not use the official in-app updater.
```

If the upstream project merges the feature, rebase once more, confirm the
upstream implementation covers all behavior above, and then stop maintaining
the duplicate downstream patch.
