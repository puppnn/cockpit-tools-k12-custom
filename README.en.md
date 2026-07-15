# Cockpit Tools K12 Custom

English · [Portuguese (BR)](README.pt-br.md) · [简体中文](README.md)

[![Custom fork](https://img.shields.io/badge/custom%20fork-K12%20session%20routing-2f81f7)](https://github.com/puppnn/cockpit-tools-k12-custom)
[![Based on](https://img.shields.io/badge/based%20on-Cockpit%20Tools%20v1.3.2-555)](https://github.com/jlcodes99/cockpit-tools/releases/tag/v1.3.2)
[![Upstream](https://img.shields.io/badge/upstream-jlcodes99%2Fcockpit--tools-238636)](https://github.com/jlcodes99/cockpit-tools)

> [!IMPORTANT]
> This is a custom fork of [jlcodes99/cockpit-tools](https://github.com/jlcodes99/cockpit-tools). It now fully integrates the upstream **v1.3.2** release while adding K12 session routing, long-running task failover, and OAuth quota reserves to the local Codex API service. Upstream features and the custom routing policy are retained together; the custom behavior is not part of official upstream releases.

A **universal AI IDE account management tool**, currently supporting **Antigravity IDE**, **Codex**, **GitHub Copilot**, **Windsurf**, **Kiro**, **Cursor**, **Grok CLI**, **CodeBuddy**, **CodeBuddy CN**, **Qoder**, **Trae**, **TRAE SOLO**, **Trae CN**, **TRAE SOLO CN**, **Zed**, and **ZCode**, with multi-instance parallel workflows.

> Designed to help users efficiently manage multiple AI IDE accounts, this tool supports one-click switching, quota monitoring, wake-up tasks, and multi-instance parallel runs, helping you fully utilize resources from different accounts.

**Features**: One-click Switch · Multi-account Management · Multi-instance · Quota Monitoring · Wake-up Tasks · Plugin Integration · GitHub Copilot Management · Windsurf Management · Kiro Management · Cursor Management · Grok CLI Management · CodeBuddy Management · CodeBuddy CN Management · Qoder Management · Trae Suite Management · Zed Management · ZCode Management

**Languages**: Supports 18 languages

🇺🇸 English · 🇨🇳 简体中文 · 繁體中文 · 🇯🇵 日本語 · 🇩🇪 Deutsch · 🇪🇸 Español · 🇫🇷 Français · 🇮🇹 Italiano · 🇰🇷 한국어 · 🇧🇷 Português · 🇷🇺 Русский · 🇹🇷 Türkçe · 🇵🇱 Polski · 🇨🇿 Čeština · 🇸🇦 العربية · 🇻🇳 Tiếng Việt · 🇮🇩 Bahasa Indonesia

**Upstream-supported platforms**: macOS, Windows, and Linux.

---

## Upstream v1.3.2 Integration

- **Full platform set**: Retains the upstream v1.3.2 Grok CLI and ZCode integrations, multi-instance management, and 18-language UI.
- **Updated Codex account experience**: Uses dynamic plan filters and quota summaries, optional model-specific quota display, clear-filter actions, improved account names, and the newer import flows.
- **API service improvements**: Keeps upstream backup accounts, import-to-API-pool support, request-log account names, and proxy connection improvements, with the custom K12 policy layered on top.
- **Custom configuration compatibility**: Preserves nullable `weeklyPercent`, the preferred pool for new sessions, and persistent K12 session state instead of replacing them with upstream defaults.

---

## Custom Fork Enhancements

### Session-Aware K12 Routing

- **Real session affinity**: Native Codex session fields are preferred. A session is confirmed on the K12 account that actually served it only after the first successful response or first successful stream payload; selection alone does not create a persistent binding.
- **Stable session identity**: Identity sources prioritize `execution_session_id`, `prompt_cache_key`, Codex turn/window metadata, and Session/Conversation headers, with Claude session fields and a message hash retained as compatibility fallbacks.
- **Established sessions keep running**: A confirmed session stays on its original K12 for as long as the upstream accepts requests, even when Cockpit displays zero remaining 5h or weekly quota for that account.
- **Quota-aware admission for new sessions**: A fresh quota snapshot showing zero 5h quota prevents only new sessions from using that K12. Existing confirmed sessions remain eligible. A missing or stale snapshot permits one real request to verify availability.
- **Parallel session distribution**: Any K12 that can accept a new session stays ahead of Plus, API-key accounts, and a preferred non-K12 pool. K12 candidates are balanced by full active-session load before remaining 5h quota and the existing custom route order are considered.
- **Persistent affinity does not consume idle capacity**: A successful K12 binding is retained for seven days only so the old session can return to its original account; an idle historical binding does not count as load. Capacity is held for the full HTTP, SSE, or WebSocket request. While paid spillover is available, each K12 carries at most two active new sessions before excess work spills to Plus. Existing affinity is never migrated merely to enforce the limit.
- **Affinity across model aliases**: A K12 binding does not include the model ID, so switching model aliases within the same Codex session keeps the original account when possible. Disabled or unsupported models still return their normal model error.
- **Non-K12 priority and concurrency balancing**: Existing in-memory affinity stays on its current account. New sessions enter the highest custom-priority tier first, with at most four active new sessions per account and least-load distribution inside an equal-priority tier. Routing falls through only when the whole tier is full, then uses lower tiers or backup accounts; an existing affinity hit is never migrated to enforce the cap.
- **Parallel consistency within one session**: Parallel requests for the same real session are never split across accounts. A failed attempt releases its provisional slot before waiting for sibling requests to finish or converge on the same target account. The wait is client-cancelable and does not hold the global routing lock.

### Preferred Account Pool for New Sessions

- Enable **Prefer New Sessions** under **API Service > Routing Options** and select one or more accounts that currently belong to the service.
- The selected pool is consulted only when a stable session identity is available and no affinity binding exists. Confirmed K12 bindings, tentative or spillover K12 bindings, and ordinary in-memory affinity hits always keep their current account. An eligible K12 also remains ahead of every selected non-K12 account.
- Changing or disabling the pool never migrates an established session. If every selected account is unavailable because of model support, quota, cooldown, disabled state, or concurrency capacity, routing falls back to the existing policy.
- The setting is hot-loaded through the sidecar state file, so changing only this pool does not restart the API service or erase ordinary in-memory affinity. Removed accounts are pruned automatically.

### Continuity and Failover

- **429 spillover without losing affinity**: When a confirmed K12 actually returns 429, eligible Plus, Team, or other paid fallback accounts can temporarily carry requests during the recovery window. The original K12 session binding remains authoritative and is retried after that window. A failed new session can still switch accounts within the same request without globally cooling the K12.
- **Persistent 402 isolation for new sessions**: After a K12 rejects a new session with 402, that account stops accepting new sessions across requests and sidecar restarts, while the current request immediately spills to Plus. Ordinary quota-related 402 responses release only the failed session and preserve unrelated confirmed sessions on the same account; an explicit `deactivated_workspace` clears all bindings for that account and enters hard isolation.
- **First-payload timeout recovery**: A stream that never produces meaningful output triggers a controlled retry or temporary paid-account spillover. A confirmed K12 session is not permanently migrated because of one timeout. The first-payload timeout is refreshed after credential failover, with no more than 60 seconds of additional total grace.
- **Failure isolation**: A failed new session does not put the entire K12 account into global cooldown or disturb other confirmed sessions on that account.
- **Hard-failure cleanup**: Bindings are removed when an account is deleted, disabled, or has clearly invalid credentials, allowing requests to move to another account.

### OAuth / Plus Quota Reserve

- Eligible Plus or Team accounts take additional concurrency once every K12 available for new sessions has reached its two-session capacity. The OAuth account bound to the API service normally remains the final fallback.
- By default, the final **10% of its 5h quota** is reserved. The account stops receiving requests at exactly 10% remaining.
- The weekly reserve can be left empty, meaning no local weekly cap. This rule applies only to the bound OAuth account; other accounts are unchanged.
- A missing or stale quota snapshot fails closed for the bound account so its reserve is not consumed when the remaining quota cannot be verified.

### Full-Quota Ping and Persistence

- **Generic full-quota ping**: Main quota slots are no longer assumed to mean 5h and 7d. An account can receive one manual minimal `ping` when any real main quota window is at 99% or 100% remaining and its countdown has not started. Every other main quota window on that account must have more than 10% remaining, so 7d-only and monthly-only accounts are supported. Model-specific additional limits are not triggered with a mismatched fixed model.
- **Predictable filtered scope**: The quota-timer button stays in the account-list action bar. Search, tag, type, and group filters limit the candidate scope; when the filtered results have no eligible account, the button is disabled instead of disappearing with the batch-selection actions.
- **Restart-safe affinity**: Successful K12 bindings use a rolling seven-day lifetime and can be restored after a sidecar restart while still valid.
- **Privacy-preserving state**: Persistent session keys are derived from the local API service key and stored as HMAC-SHA256 digests. The K12 state file contains no raw session IDs, prompts, message content, account tokens, or request logs.

> [!CAUTION]
> Cockpit quota values are snapshots, not the final authority on whether an established upstream session can continue. This fork cannot guarantee continuation to any fixed weekly percentage, such as 30%, and it does not fabricate sessions or send background keepalive requests. Once the upstream truly rejects a request, automatic account failover can keep the task running, but changing accounts may lose continuity of the original upstream session.

---

## Sponsors

<table>
  <tr>
    <td width="120" align="center">
      <a href="https://apikey.fun/register?aff=COCKPIT">
        <img src="src/assets/icons/apikey-fun.png" alt="APIKEY.FUN" width="72" />
      </a>
    </td>
    <td>
      <a href="https://apikey.fun/register?aff=COCKPIT"><strong>APIKEY.FUN</strong></a> is a professional enterprise-grade AI relay focused on stable, efficient, and low-cost AI model API access for companies and individual developers. It supports popular models such as Claude, OpenAI, and Gemini, with prices as low as 7% of official pricing. Register through this project <a href="https://apikey.fun/register?aff=COCKPIT"><strong>exclusive link</strong></a> to receive an exclusive <strong>permanent 5% top-up discount</strong>.
    </td>
  </tr>
  <tr>
    <td width="120" align="center">
      <a href="https://roxybrowser.cn?code=0326VTDA">
        <img src="src/assets/icons/roxybrowser.jpg" alt="RoxyBrowser" width="96" />
      </a>
    </td>
    <td>
      <a href="https://roxybrowser.cn?code=0326VTDA"><strong>RoxyBrowser</strong></a> is an anti-detect browser for multi-account operations and AI automation, supporting isolated browser fingerprint environments, Cookie / storage isolation, Roxy native residential IPs, team collaboration, and API / MCP automation. It helps users manage AI account matrices, reduce account association risk, and improve long-term stability. Register or purchase through the Cockpit <a href="https://roxybrowser.cn?code=0326VTDA"><strong>invite link</strong></a> to get a 10% fan discount.
    </td>
  </tr>
</table>

---

## Feature Overview

### 1. Dashboard

A brand new visual dashboard providing a one-stop status overview:

- **Sixteen-Platform Support**: Simultaneously displays Antigravity IDE, Codex, GitHub Copilot, Windsurf, Kiro, Cursor, Grok CLI, CodeBuddy, CodeBuddy CN, Qoder, Trae, TRAE SOLO, Trae CN, TRAE SOLO CN, Zed, and ZCode account status
- **Quota Monitoring**: Real-time view of remaining quotas and reset times for each model
- **Quick Actions**: One-click refresh, one-click wake-up
- **Visual Progress**: Intuitive progress bars showing quota consumption

> ![Dashboard Overview](docs/images/dashboard_overview.png)

### 2. Antigravity IDE Account Management

- **One-Click Switch**: Switch the currently active account instantly without manual login/logout
- **Multiple Import Methods**: OAuth, Refresh Token, Plugin Sync
- **Wake-up Tasks**: Schedule AI model wake-ups to trigger quota reset cycles in advance

> ![Antigravity IDE Accounts](docs/images/antigravity_list.png)
>
> *(Wakeup Tasks)*
> ![Wakeup Tasks](docs/images/wakeup_detail.png)

#### 2.1 Antigravity IDE Multi-Instance

Run multiple Antigravity IDE instances in parallel with different accounts. For example, open two Antigravity IDE instances, bind different accounts, and handle different projects independently.

- **Isolated Accounts**: Each instance binds a different account and runs independently
- **Parallel Projects**: Run multiple tasks/projects at the same time
- **Argument Isolation**: Custom instance directory and launch arguments

> ![Antigravity IDE Instances](docs/images/antigravity_instances.png)

### 3. Codex Account Management

- **Dedicated Support**: Optimized account management experience for Codex
- **Quota Display**: Clear display of Hourly and Weekly quota status
- **Plan Recognition**: Automatically identifies account Plan types (Basic, Plus, Team, etc.)
- **API Service**: The local Codex API service is powered by the bundled CLIProxyAPI sidecar. Cockpit Tools handles account sync, config projection, status, and usage statistics while keeping the same Base URL, API keys, and user workflow.

> ![Codex Accounts](docs/images/codex_list.png)

#### 3.1 Codex Multi-Instance

Codex also supports parallel multi-instance usage. For example, open two Codex instances, bind different accounts, and handle different projects independently.

- **Isolated Accounts**: Each instance binds a different account and runs independently
- **Parallel Projects**: Run multiple tasks/projects at the same time
- **Argument Isolation**: Custom instance directory and launch arguments

> ![Codex Instances](docs/images/codex_instances.png)

### 4. GitHub Copilot Account Management

- **Account Import**: OAuth, Token/JSON import
- **Quota View**: Inline Suggestions / Chat messages usage and reset time
- **Plan Recognition**: Auto-detects Free / Individual / Pro / Business / Enterprise tiers
- **Batch Operations**: Tags and bulk actions

#### 4.1 GitHub Copilot Multi-Instance

Manage VS Code Copilot instances with isolated profiles and lifecycle controls.

- **Isolated Profiles**: Each instance uses its own user data directory
- **Quick Lifecycle**: Start/stop/force stop instances
- **Window Control**: Open instance windows and close all instances

### 5. Windsurf Account Management

- **Account Import**: OAuth, Token/JSON import, and local import
- **Quota View**: Shows Plan, User Prompt credits, Add-on prompt credits, and cycle information
- **Batch Operations**: Tags and bulk actions
- **Switch Injection**: Supports injecting and launching Windsurf after account switch

#### 5.1 Windsurf Multi-Instance

Manage Windsurf instances with isolated profiles and lifecycle controls.

- **Isolated Profiles**: Each instance uses its own user data directory
- **Quick Lifecycle**: Start/stop/force stop instances
- **Window Control**: Open instance windows and close all instances

### 6. Kiro Account Management

- **Account Import**: OAuth, Token/JSON import, and local import
- **Quota View**: Shows Plan, User Prompt credits, Add-on prompt credits, and cycle information
- **Batch Operations**: Tags and bulk actions
- **Switch Injection**: Supports injecting and launching Kiro after account switch

#### 6.1 Kiro Multi-Instance

Manage Kiro instances with isolated profiles and lifecycle controls.

- **Isolated Profiles**: Each instance uses its own user data directory
- **Quick Lifecycle**: Start/stop/force stop instances
- **Window Control**: Open instance windows and close all instances

### 7. Cursor Account Management

- **Account Import**: OAuth, Token/JSON import, and local import
- **Quota View**: Shows Total Usage, Auto + Composer, API Usage, On-Demand, and cycle information
- **Batch Operations**: Tags and bulk actions
- **Switch Injection**: Supports injecting and launching Cursor after account switch

#### 7.1 Cursor Multi-Instance

Manage Cursor instances with isolated profiles and lifecycle controls.

- **Isolated Profiles**: Each instance uses its own user data directory
- **Quick Lifecycle**: Start/stop/force stop instances
- **Window Control**: Open instance windows and close all instances


### 8. Grok CLI Account Management

- **OAuth Authorization**: Supports xAI's official OIDC device flow and saves the account after browser verification completes
- **Import and Redacted Export**: Imports official credentials from the default `~/.grok/auth.json` or supplied JSON; account-page exports and generic account backups omit access/refresh tokens, cannot restore a sign-in, and require a separate official `auth.json` import when migrating
- **Real Account Switching**: Writes the selected account to the default `~/.grok/auth.json` in Grok CLI's official registry format while preserving other registry scopes in the file
- **Quota and Plan**: Queries the official billing/user/subscriptions endpoints, displays cycle, usage, product quotas, and the raw plan value, and records Grok Code access
- **Token Maintenance**: Supports automatic access-token refresh, refresh-token rotation, and quota alerts

#### 8.1 Grok CLI Multi-Instance

The default Grok CLI instance uses the official `~/.grok` directory directly and starts without setting `GROK_HOME`. Only managed instances use separate directories, with an independent `GROK_HOME` set for each instance.

- **Account Binding**: The default instance can follow the current account, while each managed instance can bind a different account
- **Runtime Isolation**: Managed instances keep their `auth.json`, working directories, and launch arguments separate
- **Terminal Lifecycle**: Generate or execute terminal launch commands, stop instances, and close all instances
- **Directory Protection**: Non-default instances are confined to the default managed root and moved to the trash when deleted; external paths from legacy configuration are only unregistered and are never written to or deleted

### 9. CodeBuddy Account Management

- **Account Import**: OAuth and Token/JSON import
- **Quota View**: quota query, cycle details, and extra-credit display
- **Batch Operations**: tags and bulk actions
- **Switch Injection**: supports injecting and launching CodeBuddy after account switch

#### 8.1 CodeBuddy Multi-Instance

Manage CodeBuddy instances with isolated profiles and lifecycle controls.

- **Isolated Profiles**: Each instance uses its own user data directory
- **Quick Lifecycle**: Start/stop/force stop instances
- **Window Control**: Open instance windows and close all instances

### 10. CodeBuddy CN Account Management

- **Account Import**: supports OAuth, Token/JSON import, and local-client import
- **Quota View**: shows plan and usage status, with a shortcut to open detailed quota information on the official web page
- **Batch Operations**: supports tags and bulk actions
- **Switch Injection**: supports writing local auth state back and launching CodeBuddy CN after account switch

#### 9.1 CodeBuddy CN Multi-Instance

Manage CodeBuddy CN instances with isolated profiles and lifecycle controls.

- **Isolated Profiles**: each instance uses its own user data directory
- **Quick Lifecycle**: start/stop/force stop instances
- **Window Control**: open instance windows and close all instances

### 11. Qoder Account Management

- **Account Import**: supports local import and JSON import
- **Quota View**: shows Credits usage, remaining credits, and raw plan values
- **Batch Operations**: supports tags, filters, export, and batch delete/refresh
- **Switch Injection**: supports injecting and launching Qoder after account switch

#### 10.1 Qoder Multi-Instance

Manage Qoder instances with isolated profiles and lifecycle controls.

- **Isolated Profiles**: each instance uses its own user data directory
- **Quick Lifecycle**: start/stop/force stop instances
- **Window Control**: open instance windows and close all instances

### 12. Trae Account Management

- **Account Import**: supports local import and JSON import
- **Quota View**: shows raw plan values, USD spent/total budget, and reset time
- **Batch Operations**: supports tags, filters, export, and batch delete/refresh
- **Trae Suite**: supports local import and switch injection for the default clients of Trae, TRAE SOLO, Trae CN, and TRAE SOLO CN; they are grouped under Trae by default
- **Switch Injection**: supports writing back local auth state using each client's real on-disk rules and launching the target client

#### 11.1 Trae Multi-Instance

Manage original Trae client instances with isolated profiles and lifecycle controls.

- **Isolated Profiles**: each instance uses its own user data directory
- **Quick Lifecycle**: start/stop/force stop instances
- **Window Control**: open instance windows and close all instances

### 13. Zed Account Management

- **Account Import**: Supports official OAuth sign-in, JSON import, and importing the current local sign-in state
- **Usage View**: Shows subscription status, Edit Predictions, Token Spend, Spend Limit, and billing period end
- **Batch Operations**: Supports tags, filters, export, and batch delete/refresh
- **Switch Injection**: Applies the selected account back to the official Zed client using the client's real local persistence rules and restarts the client when needed

### 14. ZCode Account Management

- **Official Sign-in**: With ZCode closed, complete Z.ai or BigModel OAuth in Cockpit's built-in authorization window; it captures the official `zcode://` callback directly and saves the account
- **Import and Export**: Read encrypted local credentials from `~/.zcode/v2/credentials.json`, import or export JSON, and back up accounts
- **Quota View**: Query subscription plans and per-model quotas while preserving raw plan values
- **Batch Operations**: Tags, search, plan filters, export, and batch delete/refresh
- **Real Account Switching**: Encrypt and write the selected account back using ZCode's official credential format

#### 13.1 ZCode Multi-Instance

Manage ZCode instances with separate Electron user data, session data, and ZCode data directories.

- **Account Binding**: Bind a different account to each instance or follow the current account
- **Isolated Runtime**: Instance credentials and application data remain separate
- **Lifecycle Controls**: Start, stop, focus, and close all managed instances

### 15. General Settings

- **Personalized Settings**: Theme switching, language settings, auto-refresh interval
- **Platform Controls**: Centralized Grok CLI/CodeBuddy CN/Qoder/Trae suite/Zed/ZCode platform and quota-alert settings

> ![Settings](docs/images/settings_page.png)

---

## Security & Privacy (Plain-English)

These are the most common security questions answered directly:

- **This is a local desktop tool**: it does not require a separate cloud account for this project, and it does not rely on a project-hosted cloud account storage.
- **Data is mainly stored on your machine**:
  - `~/.antigravity_cockpit`: Antigravity IDE accounts, configs, WebSocket status, etc.
  - `~/.codex`: official Codex current login `auth.json`
  - `~/.grok`: the official Grok CLI default instance and current sign-in `auth.json`
  - `~/.zcode/v2`: ZCode encrypted credentials for the current official sign-in and quota cache
  - local app data folder under `com.antigravity.cockpit-tools`: Codex / GitHub Copilot / Windsurf / Kiro / Cursor / Grok CLI / CodeBuddy / CodeBuddy CN / Qoder / Trae suite / Zed / ZCode multi-account data, etc.; Grok CLI account details, managed profiles, and instance configuration are also stored here
- **Grok CLI credentials are not encrypted**: access and refresh tokens are stored locally as plaintext JSON and rely primarily on operating-system account isolation and local file permissions. On Unix systems, credential directories are set to `0700` and credential files to `0600`. Redacted exports contain no tokens and cannot serve as sign-in backups.
- **WebSocket is local-only by default**: binds to `127.0.0.1`, default port `19528`; you can disable it or change the port in Settings.
- **When network access happens**: OAuth login, token refresh, quota fetching, update checks, and other official API requests.
- **macOS privacy permission prompts**: after you start Codex/agent from Cockpit Tools, if an agent-run shell command accesses protected folders such as Desktop, Documents, Downloads, or Photos, macOS may show the request as "Cockpit Tools would like to access...". This happens because those commands are child processes launched by Cockpit Tools, so macOS attributes the request to the host app; it does not by itself mean the Cockpit Tools main process is actively scanning those folders. Grant access only when you trust the current agent task and the commands it is going to run. If unsure, deny the prompt or run the project from a normal working directory first.
- **Practical safety tips**:
  1. If you do not need plugin integration, disable WebSocket.
  2. Do not share your full user directory directly; redact token files before backup/share.
  3. On shared/public computers, remove accounts and quit the app after use.

## Settings Guide (Beginner Friendly)

If you want a stable setup with minimal tuning, follow the "Recommended" values.

### General Settings

| Setting | What it does (simple) | Recommended | When to change |
| --- | --- | --- | --- |
| Display Language | Changes UI language | Your native/comfortable language | Only if current language is hard to read |
| Theme | Light/dark appearance | System | Use dark mode for long night sessions |
| Window Close Behavior | What happens when clicking close | Ask every time | Choose "Minimize to tray" if you want background running |
| Antigravity IDE Auto Refresh | Periodically updates Antigravity IDE quota | 5-10 minutes | Use 2 minutes if you need near real-time updates |
| Codex Auto Refresh | Periodically updates Codex quota | 5-10 minutes | Same as above |
| GitHub Copilot Auto Refresh | Periodically updates GitHub Copilot quota | 5-10 minutes | Same as above |
| Windsurf Auto Refresh | Periodically updates Windsurf quota | 5-10 minutes | Same as above |
| Kiro Auto Refresh | Periodically updates Kiro quota | 5-10 minutes | Same as above |
| Cursor Auto Refresh | Periodically updates Cursor quota | 5-10 minutes | Same as above |
| Grok CLI Auto Refresh | Periodically refreshes tokens and updates quota | 5-10 minutes | Same as above |
| CodeBuddy Auto Refresh | Periodically updates CodeBuddy quota | 5-10 minutes | Same as above |
| CodeBuddy CN Auto Refresh | Periodically updates CodeBuddy CN quota | 5-10 minutes | Same as above |
| Qoder Auto Refresh | Periodically updates Qoder quota | 5-10 minutes | Same as above |
| Trae Auto Refresh | Periodically updates Trae suite account quota | 5-10 minutes | Same as above |
| Zed Auto Refresh | Periodically updates Zed quota | 5-10 minutes | Same as above |
| Data Directory | Where account/config files are stored | Keep default | Only for troubleshooting or backups |
| Antigravity IDE/Codex/VS Code/Windsurf/Kiro/Cursor/Grok CLI/CodeBuddy/CodeBuddy CN/Qoder/Trae/Zed/OpenCode App Path | Manually set executable path | Leave empty (auto-detect) | Change only if auto-detect fails or you use custom install paths |
| Auto-restart OpenCode on Codex switch | Sync OpenCode auth after Codex switch | ON if you use OpenCode; otherwise OFF | Enable for frequent Codex switching with OpenCode |

Notes:
- Smaller refresh intervals mean more frequent requests.
- If quota-reset wake-up tasks are enabled, some minimum refresh limits may apply (UI will show hints).

### Network Settings

| Setting | What it does (simple) | Recommended | Risk / Notes |
| --- | --- | --- | --- |
| WebSocket Service | Real-time local integration for plugins/clients | OFF if not needed | Still local-only (`127.0.0.1`) when enabled |
| Preferred Port | Listening port for WebSocket | Default `19528` | Change only on conflict; restart required after save |
| Current Running Port | The actual active port | Read-only info | May differ if preferred port is occupied |

### 3 Ready-to-Use Presets

1. **Stable default**: 10-min refresh, WebSocket OFF (if no plugin), keep default paths.  
2. **Frequent switching**: 2-5 min refresh, WebSocket ON if needed, OpenCode sync ON.  
3. **Security-first**: WebSocket OFF, do not share user directory, remove unused accounts regularly.  

---



---

## Installation Guide

### Option A: Manual Download (Recommended)

Go to [GitHub Releases](https://github.com/jlcodes99/cockpit-tools/releases) to download the package for your system:

*   **macOS**: `.dmg` (Apple Silicon & Intel)
*   **Windows**: `.msi` (Recommended) or `.exe`
*   **Linux**: `.deb` (Debian/Ubuntu), `.rpm`, or `.AppImage` (Universal)

### Option B: Install with Homebrew (macOS)

> Homebrew is required.

```bash
brew tap jlcodes99/cockpit-tools https://github.com/jlcodes99/cockpit-tools
brew install --cask cockpit-tools
```

If you hit the macOS "App is damaged" warning, you can also install with `--no-quarantine`:

```bash
brew install --cask --no-quarantine cockpit-tools
```

If Homebrew says the app already exists (e.g. `already an App at '/Applications/Cockpit Tools.app'`), remove the old app and install again:

```bash
rm -rf "/Applications/Cockpit Tools.app"
brew install --cask cockpit-tools
```

Or force overwrite the existing app:

```bash
brew install --cask --force cockpit-tools
```

### 🛠️ Troubleshooting

#### macOS says "App is damaged and can't be opened"?
Due to macOS security mechanisms, apps not downloaded from the App Store may trigger this warning. The current open-source release flow does not yet use Apple Developer ID signing or notarization, so some macOS versions may show stricter Gatekeeper prompts. You can quickly fix this by following these steps:

1.  **Command Line Fix** (Recommended):
    Open Terminal and run the following command:
    ```bash
    sudo xattr -rd com.apple.quarantine "/Applications/Cockpit Tools.app"
    ```
    > **Note**: If you changed the app name, please adjust the path in the command accordingly.

2.  **Or**: Go to "System Settings" -> "Privacy & Security" and click "Open Anyway".

---

## Development & Build

### Prerequisites

- Node.js v18+
- npm v9+
- Rust (Tauri runtime)

### Install Dependencies

```bash
npm install
```

### Development Mode

```bash
npm run tauri dev
```

### Build

```bash
npm run tauri build
```

---

## Star History

[![Star History Chart](https://api.star-history.com/svg?repos=jlcodes99/cockpit-tools&type=Date)](https://star-history.com/#jlcodes99/cockpit-tools&Date)

---

## Community

Newly created Telegram chat group: [Join the group](https://t.me/+Y8gMv4SlZUU2MWY1)

---

## Sponsor

If you find this project useful, consider supporting it here: [☕ Donate](docs/DONATE.en.md)

Every bit of support helps sustain open-source development. Thank you!

---

## Acknowledgments

- Antigravity account switching logic references: [Antigravity-Manager](https://github.com/lbjlaq/Antigravity-Manager)
- The Codex API service integrates CLIProxyAPI, and its open-source account and OAuth handling also informed the Grok CLI implementation: [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (MIT)
- Grok icon shape references: [LobeHub/lobe-icons](https://github.com/lobehub/lobe-icons) (MIT)
- Grok CLI task-usage querying and compatibility parsing direction references: [junhoyeo/tokscale](https://github.com/junhoyeo/tokscale) (MIT)
- Codex API service protocol compatibility direction references: [codex-proxy](https://github.com/icebear0828/codex-proxy)
- Codex, Claude CLI, and Claude Desktop Gateway third-party provider presets and model mapping direction reference: [CC Switch](https://github.com/farion1231/cc-switch)
- Codex model catalog and frontend model display ideas reference: [CodexPlusPlus](https://github.com/BigPizzaV3/CodexPlusPlus)
- Claude optional sign-in helper runtime is based on: [Electron](https://github.com/electron/electron)
- Thanks [@longwQaQ](https://github.com/longwQaQ) for contributing per-provider Codex Responses WebSocket configuration ([#1512](https://github.com/jlcodes99/cockpit-tools/pull/1512)).

Thanks to the project author for their open-source contributions! If these projects have helped you, please give them a ⭐ Star to show your support!

---

## License

This project is licensed under [CC BY-NC-SA 4.0](https://creativecommons.org/licenses/by-nc-sa/4.0/).

- Allowed: personal learning, research, and non-commercial use/modification (with attribution and share-alike obligations).
- Not allowed: any commercial use without authorization (including internal commercial operations, external paid services, paid product integration, or resale/redistribution for profit).
- Commercial license: contact the author for a separate written commercial license and pricing.

---

## Disclaimer

This project is for personal learning and research purposes only. By using this project, you agree to:

- Not use this project for any commercial purposes without prior written authorization from the author
- Bear all risks and responsibilities of using this project
- Comply with relevant terms of service and laws and regulations

The project author is not responsible for any direct or indirect losses arising from the use of this project.
