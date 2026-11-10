import { describe, expect, it } from "vitest";
import { DEFAULT_POSTHOG_PROJECT_KEY } from "./posthog-config";
import { DEFAULT_DAEMON_SENTRY_DSN, DEFAULT_ELECTRON_SENTRY_DSN } from "./sentry-config";

// Swarm 3.0 is a fork. A packaged build must not send analytics or error
// reports to the upstream project's PostHog or Sentry accounts by default.
describe("Swarm 3.0 telemetry defaults", () => {
	it("has no built-in analytics or error-report destination", () => {
		expect(DEFAULT_POSTHOG_PROJECT_KEY).toBe("");
		expect(DEFAULT_DAEMON_SENTRY_DSN).toBe("");
		expect(DEFAULT_ELECTRON_SENTRY_DSN).toBe("");
	});
});
