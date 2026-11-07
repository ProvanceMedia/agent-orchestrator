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
| Updates | The updater looks for releases in `ProvanceMedia/agent-orchestrator`, not in the upstream repository. | `frontend/forge.config.ts` |
| Telemetry | The fork has no built-in PostHog key or Sentry address, so a packaged build sends no analytics or error reports. | `frontend/src/shared/posthog-config.ts`, `frontend/src/shared/sentry-config.ts` |
| Tests | The tests expect the new name and telemetry defaults. | `frontend/forge.config.test.ts`, `frontend/src/main/sentry-main.test.ts`, `frontend/src/shared/fork-telemetry.test.ts` |

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
