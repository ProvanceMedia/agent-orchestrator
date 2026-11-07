# Swarm 3.0

Swarm 3.0 is a fork of [Agent Orchestrator](https://github.com/OrchestratorInc/agent-orchestrator)
by Orchestrator.inc. The original project and this fork use the Apache License 2.0. See `LICENSE`.

Swarm 3.0 is not made, approved or supported by Orchestrator.inc.

## Changes in this fork

The fork changes these files. The git history on branch `feat/swarm-3-branding` holds each change.

| Area | Change | Files |
|---|---|---|
| Name | The desktop app is called "Swarm 3.0", with the bundle ID `com.provancemedia.swarm3`. | `frontend/forge.config.ts`, `frontend/src/main.ts`, `frontend/package.json`, `backend/internal/cli/start.go` |
| Icons | The app icon is the Swarm wasp mark. | `frontend/assets/icon.icns`, `frontend/assets/icon.png`, `frontend/assets/icon-dev.png` |
| About label | The About menu item names Swarm 3.0 in each language. | `frontend/src/renderer/i18n/*.json` |
| Updates | The updater looks for releases in `ProvanceMedia/agent-orchestrator`, not in the upstream repository. | `frontend/forge.config.ts` |
| Telemetry | The fork has no built-in PostHog key or Sentry address, so a packaged build sends no analytics or error reports. | `frontend/src/shared/posthog-config.ts`, `frontend/src/shared/sentry-config.ts` |
| Tests | The tests expect the new name and telemetry defaults. | `frontend/forge.config.test.ts`, `frontend/src/main/sentry-main.test.ts`, `frontend/src/shared/fork-telemetry.test.ts` |

## Not changed yet

- The app keeps its data in `~/.ao`, the same folder as Agent Orchestrator. Do not run both apps at the same time.
- Cloud features, account sign-in, remote hosts and the mobile app use Orchestrator.inc's servers and a private submodule. They do not work as Swarm features.
- Some text in the app still names Orchestrator.inc, for example the sign-in and sharing text for those cloud features.
- The first-run mascot, the executable name `agent-orchestrator` and the Windows and Linux installers are not changed.

## Build

```sh
npm ci
cd frontend
npm ci
npm run package
```

The app is in `frontend/out/Swarm 3.0-darwin-arm64/`. The build needs Go 1.27.1 or later and Node 24 or later.
