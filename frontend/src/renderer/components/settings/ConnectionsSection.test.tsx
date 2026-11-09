import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const apiMocks = vi.hoisted(() => ({ get: vi.fn(), put: vi.fn(), post: vi.fn(), del: vi.fn() }));
const bridgeMocks = vi.hoisted(() => ({ openExternal: vi.fn() }));

vi.mock("../../lib/api-client", () => ({
	apiClient: { GET: apiMocks.get, PUT: apiMocks.put, POST: apiMocks.post, DELETE: apiMocks.del },
	apiErrorMessage: (error: unknown, fallback = "Request failed") =>
		typeof error === "object" && error && "message" in error ? String((error as { message: unknown }).message) : fallback,
	apiErrorCode: (error: unknown) =>
		typeof error === "object" && error && "code" in error ? String((error as { code: unknown }).code) : undefined,
}));

vi.mock("../../lib/bridge", () => ({ aoBridge: { app: bridgeMocks } }));

import { ConnectionsSection } from "./ConnectionsSection";

type Connection = Record<string, unknown> & { id: string; name: string; connected: boolean };

const cloudflare: Connection = {
	id: "cloudflare",
	name: "Cloudflare",
	tool: "cloudflare_api",
	blurb: "DNS, zones, Workers, Email Routing, Registrar.",
	tint: "#f6821f",
	keyUrl: "https://dash.cloudflare.com/profile/api-tokens",
	fields: [{ key: "key", label: "API token", placeholder: "a scoped API token, not the global key", secret: true }],
	connected: false,
};
const posthog: Connection = {
	id: "posthog",
	name: "PostHog",
	tool: "posthog_api",
	blurb: "HogQL queries, insights, flags, session recordings.",
	tint: "#f9bd2b",
	keyUrl: "https://app.posthog.com/settings/user-api-keys",
	fields: [
		{ key: "key", label: "Personal API key", placeholder: "phx_…", secret: true },
		{ key: "host", label: "Host", placeholder: "https://eu.posthog.com" },
	],
	connected: true,
	tail: "a1b2",
	values: { host: "https://eu.posthog.com" },
	connectedAt: "2026-11-01T10:00:00Z",
};

function daemon(list: () => { available: boolean; connections: Connection[] }) {
	apiMocks.get.mockImplementation(async (path: string) => {
		if (path === "/api/v1/connections") return { data: list(), error: undefined };
		return { data: undefined, error: { message: `unexpected ${path}` } };
	});
}

function renderSection() {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	return render(
		<QueryClientProvider client={client}>
			<ConnectionsSection />
		</QueryClientProvider>,
	);
}

/** The row of one service: its disclosure button and its form. */
async function openRow(name: string) {
	const toggle = await screen.findByRole("button", { name: new RegExp(`^${name}`) });
	await userEvent.click(toggle);
	expect(toggle).toHaveAttribute("aria-expanded", "true");
	const row = toggle.closest<HTMLElement>("[data-connection]");
	if (!row) throw new Error(`no row for ${name}`);
	return row;
}

beforeEach(() => {
	apiMocks.get.mockReset();
	apiMocks.put.mockReset();
	apiMocks.post.mockReset();
	apiMocks.del.mockReset();
	bridgeMocks.openExternal.mockReset().mockResolvedValue(undefined);
});

describe("ConnectionsSection", () => {
	it("lists connected services first with the key's tail, then the others", async () => {
		daemon(() => ({ available: true, connections: [cloudflare, posthog] }));
		renderSection();

		expect(await screen.findByText("Swarm keeps the keys in the macOS keychain. Agents use a tool to call each service, and they never see the key.")).toBeInTheDocument();
		const rows = await screen.findAllByRole("button", { expanded: false });
		expect(rows.map((row) => row.closest("[data-connection]")?.getAttribute("data-connection"))).toEqual(["posthog", "cloudflare"]);
		expect(within(rows[0]).getByText("Connected")).toBeInTheDocument();
		expect(within(rows[0]).getByText("…a1b2")).toBeInTheDocument();
		expect(within(rows[0]).getByText("The saved key ends in a1b2.")).toBeInTheDocument();
		expect(within(rows[1]).getByText("Not connected")).toBeInTheDocument();
		expect(within(rows[1]).getByText("DNS, zones, Workers, Email Routing, Registrar.")).toBeInTheDocument();
	});

	it("connects with the pasted key, shows the service's answer and clears the key", async () => {
		const connected = { ...cloudflare, connected: true, tail: "wxyz", values: {} };
		let list = [cloudflare];
		daemon(() => ({ available: true, connections: list }));
		apiMocks.put.mockImplementation(async () => {
			list = [connected];
			return { data: { ok: true, detail: "connected", connection: connected }, error: undefined };
		});
		renderSection();

		const row = await openRow("Cloudflare");
		const key = within(row).getByLabelText("API token");
		expect(key).toHaveAttribute("type", "password");
		expect(key).toHaveAttribute("autocomplete", "off");
		const connect = within(row).getByRole("button", { name: "Connect" });
		expect(connect).toBeDisabled();

		await userEvent.type(key, "  cf-secret-token  ");
		await userEvent.click(connect);

		expect(apiMocks.put).toHaveBeenCalledWith("/api/v1/connections/{id}", {
			params: { path: { id: "cloudflare" } },
			body: { values: { key: "cf-secret-token" } },
		});
		expect(await within(row).findByText("Connected", { selector: "p" })).toBeInTheDocument();
		expect(key).toHaveValue("");
		// The row turns connected in place and offers the connected actions.
		expect(within(row).getByText("…wxyz")).toBeInTheDocument();
		expect(within(row).getByRole("button", { name: "Save" })).toBeInTheDocument();
		expect(within(row).getByRole("button", { name: "Test" })).toBeInTheDocument();
	});

	it("shows the service's reason when it rejects the key, and still clears the key", async () => {
		daemon(() => ({ available: true, connections: [cloudflare] }));
		apiMocks.put.mockResolvedValue({ data: { ok: false, detail: "the API rejected the key", connection: cloudflare }, error: undefined });
		renderSection();

		const row = await openRow("Cloudflare");
		const key = within(row).getByLabelText("API token");
		await userEvent.type(key, "bad-token{Enter}");

		expect(await within(row).findByText("The API rejected the key")).toHaveClass("text-error");
		expect(within(row).getByText("Swarm did not save the key.")).toBeInTheDocument();
		expect(key).toHaveValue("");
		expect(within(row).getByText("Not connected")).toBeInTheDocument();
	});

	it("keeps the saved key when only a non-secret field changes", async () => {
		daemon(() => ({ available: true, connections: [posthog] }));
		apiMocks.put.mockResolvedValue({ data: { ok: true, detail: "connected", connection: { ...posthog, values: { host: "https://us.posthog.com" } } }, error: undefined });
		renderSection();

		const row = await openRow("PostHog");
		expect(within(row).getByText("Leave this empty to keep the saved key.")).toBeInTheDocument();
		const save = within(row).getByRole("button", { name: "Save" });
		expect(save).toBeDisabled();
		const host = within(row).getByLabelText("Host");
		expect(host).toHaveValue("https://eu.posthog.com");
		await userEvent.clear(host);
		await userEvent.type(host, "https://us.posthog.com");
		await userEvent.click(save);

		expect(apiMocks.put).toHaveBeenCalledWith("/api/v1/connections/{id}", {
			params: { path: { id: "posthog" } },
			body: { values: { key: "", host: "https://us.posthog.com" } },
		});
	});

	it("tests a saved key and reports a daemon error in words", async () => {
		daemon(() => ({ available: true, connections: [posthog] }));
		apiMocks.post.mockResolvedValue({ data: undefined, error: { code: "CONNECTION_KEYCHAIN_DENIED", message: "the keychain refused access" } });
		renderSection();

		const row = await openRow("PostHog");
		await userEvent.click(within(row).getByRole("button", { name: "Test" }));

		expect(apiMocks.post).toHaveBeenCalledWith("/api/v1/connections/{id}/test", { params: { path: { id: "posthog" } } });
		expect(await within(row).findByRole("alert")).toHaveTextContent("macOS did not give Swarm access to the keychain. Allow access in the keychain prompt, then try again.");
	});

	it("disconnects only after confirmation", async () => {
		let list = [posthog];
		daemon(() => ({ available: true, connections: list }));
		apiMocks.del.mockImplementation(async () => {
			list = [{ ...posthog, connected: false, tail: undefined, values: undefined, connectedAt: undefined }];
			return { data: undefined, error: undefined, response: { ok: true, status: 204 } };
		});
		renderSection();

		const row = await openRow("PostHog");
		await userEvent.click(within(row).getByRole("button", { name: "Disconnect" }));
		const dialog = await screen.findByRole("dialog", { name: "Disconnect PostHog?" });
		expect(within(dialog).getByText("Swarm deletes the key from the macOS keychain. Agents can no longer use the posthog_api tool.")).toBeInTheDocument();
		expect(apiMocks.del).not.toHaveBeenCalled();

		await userEvent.click(within(dialog).getByRole("button", { name: "Disconnect" }));

		expect(apiMocks.del).toHaveBeenCalledWith("/api/v1/connections/{id}", { params: { path: { id: "posthog" } } });
		await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
		expect(within(row).getByText("Not connected")).toBeInTheDocument();
		expect(within(row).getByRole("button", { name: "Connect" })).toBeInTheDocument();
	});

	it("opens the service's key page outside the app", async () => {
		daemon(() => ({ available: true, connections: [cloudflare] }));
		renderSection();

		const row = await openRow("Cloudflare");
		await userEvent.click(within(row).getByRole("button", { name: "Get a key" }));

		expect(bridgeMocks.openExternal).toHaveBeenCalledWith("https://dash.cloudflare.com/profile/api-tokens");
	});

	it("says so and disables the forms when this build cannot keep keys", async () => {
		daemon(() => ({ available: false, connections: [cloudflare, posthog] }));
		renderSection();

		expect(await screen.findByText("This build cannot keep keys. Connections need the macOS keychain.")).toBeInTheDocument();
		const cloudflareRow = await openRow("Cloudflare");
		expect(within(cloudflareRow).getByLabelText("API token")).toBeDisabled();
		expect(within(cloudflareRow).getByRole("button", { name: "Connect" })).toBeDisabled();
		const posthogRow = await openRow("PostHog");
		expect(within(posthogRow).getByLabelText("Host")).toBeDisabled();
		expect(within(posthogRow).getByRole("button", { name: "Test" })).toBeDisabled();
		expect(within(posthogRow).getByRole("button", { name: "Disconnect" })).toBeDisabled();
	});
});
