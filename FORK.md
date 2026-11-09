# Swarm 3.0

Swarm 3.0 is a fork of [Agent Orchestrator](https://github.com/OrchestratorInc/agent-orchestrator)
by Orchestrator.inc. The original project and this fork use the Apache License 2.0. See `LICENSE`.

Swarm 3.0 is not made, approved or supported by Orchestrator.inc.

## Changes in this fork

The fork changes these files. The git history on branch `feat/swarm-3-branding` holds each change.

| Area | Change | Files |
|---|---|---|
| Name | The desktop app is called "Swarm 3.0", with the bundle ID `com.provancemedia.swarm3`. | `frontend/forge.config.ts`, `frontend/src/main.ts`, `frontend/package.json`, `backend/internal/cli/start.go` |
| Icons and mark | The app, Windows, menu-bar and QR icons and the in-app mark use the Swarm wasp mark. | `frontend/assets/icon.icns`, `frontend/assets/icon.png`, `frontend/assets/icon-dev.png` |
| Text | All visible product names say Swarm, in all 9 languages, the CLI help and the agent instructions. | `frontend/src`, `backend` |
| Updates | The updater looks for releases in `ProvanceMedia/agent-orchestrator`, not in the upstream repository. That repository has no releases, so a check reports "no published releases" and not a failure. New versions come as a local install. | `frontend/forge.config.ts`, `frontend/src/main/auto-updater.ts`, `frontend/src/shared/update-error-message.ts` |
| Telemetry | The fork has no built-in PostHog key or Sentry address, so a packaged build sends no analytics or error reports. | `frontend/src/shared/posthog-config.ts`, `frontend/src/shared/sentry-config.ts` |
| Tests | The tests expect the new name and telemetry defaults. | `frontend/forge.config.test.ts`, `frontend/src/main/sentry-main.test.ts`, `frontend/src/shared/fork-telemetry.test.ts` |
| Connections | Settings > Connections keeps the API keys of 17 services in the macOS login keychain. Each connected service gives chat agents one `ao mcp` tool, such as `cloudflare_api`. The daemon adds the key to each call, so the agent never sees it. The catalogue and the GitHub refusal rules come from Swarm Online. The routes are loopback only. | `backend/internal/service/connections`, `backend/internal/adapters/keychain`, `backend/internal/httpd/controllers/connections.go`, `backend/internal/cli/mcp_connections.go`, `frontend/src/renderer/components/settings` |
| MCP connections | Settings > Connections also adds any remote MCP server by its URL, with a key, headers or an OAuth sign-in, and grants it to all projects or to chosen ones. Key-based services get the same project grants. The daemon keeps every credential (rows sealed with a keychain key), and each granted remote tool is a real `ao mcp` tool, such as `sanity__query`. The chat prompt names what each project can use. It is a port of Swarm Online's `src/server/mcp` (2026-11-06). | `backend/internal/service/mcpconn`, `backend/internal/httpd/controllers/mcp_connections.go`, `backend/internal/cli/mcp_mcpconn.go`, `frontend/src/renderer/components/settings` |
| Run scope | Before it starts work that ends in a PR, the orchestrator asks one question: take it all the way (Swarm review, merge when CI passes, release, check the deploy) or stop at the PR. After the answer, it does not ask again at each step. It still stops for deletes, billing, secrets, blocked merges, repeated failures and failed deploys. | `backend/internal/session_manager/prompt.go` |

## Not changed yet

- The app keeps its data in `~/.ao`, the same folder as Agent Orchestrator. Do not run both apps at the same time.
- Cloud features, account sign-in, remote hosts and the mobile app use Orchestrator.inc's servers and a private submodule. The interface calls them Swarm Cloud, but they still depend on those servers.
- The role word "orchestrator" for the coordinating agent stays, by the owner's decision.
- Functional names stay: the `ao` command, `AO_*` environment variables, paths, URLs, error codes, and text that other code matches.
- The executable name `agent-orchestrator` stays. `scripts/generate-awake-tray-icon.swift` still builds tray icons from the old mascot, so do not run it.
- The documentation site (`frontend/src/docs`), `packages/mobile` and `backend/internal/skillassets` are not rebranded.

## Build

```sh
npm ci
cd frontend
npm ci
npm run package
```

The app is in `frontend/out/Swarm 3.0-darwin-arm64/`. The build needs Go 1.27.1 or later and Node 24 or later.
