import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const apiMocks = vi.hoisted(() => ({ get: vi.fn(), put: vi.fn(), post: vi.fn(), del: vi.fn(), patch: vi.fn() }));
const bridgeMocks = vi.hoisted(() => ({ openExternal: vi.fn() }));

vi.mock("../../lib/api-client", () => ({
	apiClient: { GET: apiMocks.get, PUT: apiMocks.put, POST: apiMocks.post, DELETE: apiMocks.del, PATCH: apiMocks.patch },
	apiErrorMessage: (error: unknown, fallback = "Request failed") =>
		typeof error === "object" && error && "message" in error ? String((error as { message: unknown }).message) : fallback,
	apiErrorCode: (error: unknown) =>
		typeof error === "object" && error && "code" in error ? String((error as { code: unknown }).code) : undefined,
}));

vi.mock("../../lib/bridge", () => ({ aoBridge: { app: bridgeMocks } }));

import { McpConnectionsSection, SIGN_IN_POLL_MS } from "./McpConnectionsSection";

type Conn = Record<string, unknown> & { id: string; name: string };

function conn(partial: Partial<Conn> & { id: string; name: string }): Conn {
	return {
		url: "https://mcp.acme.test/mcp",
		urlHasSecret: false,
		authKind: "none",
		headerNames: [],
		secrets: {},
		tools: [],
		health: "draft",
		projects: null,
		createdAt: 1,
		updatedAt: 1,
		...partial,
	};
}

const search = { name: "search", description: "Search the docs.", inputSchema: {} };
const fetchTool = { name: "fetch", description: "Read one page.", inputSchema: {} };

const docsApp = {
	key: "do-docs",
	name: "DigitalOcean Docs",
	description: "Search the DigitalOcean documentation.",
	url: "https://docs.mcp.digitalocean.com/mcp",
	auth: "oauth",
	guidance: "Sign in with DigitalOcean in your browser.",
	tint: "#0080ff",
};

function daemon(list: () => Conn[], extra: { projects?: string[]; available?: boolean } = {}) {
	apiMocks.get.mockImplementation(async (path: string) => {
		if (path === "/api/v1/mcp/connections") {
			return {
				data: {
					available: extra.available ?? true,
					connections: list(),
					store: { broken: false, unreadable: 0 },
					apps: [docsApp],
					projects: extra.projects ?? ["api", "web"],
				},
				error: undefined,
			};
		}
		return { data: undefined, error: { message: `unexpected ${path}` } };
	});
}

const overviewReads = () => apiMocks.get.mock.calls.filter(([path]) => path === "/api/v1/mcp/connections").length;

function renderSection() {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	render(
		<QueryClientProvider client={client}>
			<McpConnectionsSection />
		</QueryClientProvider>,
	);
	return client;
}

async function startCustom(user: ReturnType<typeof userEvent.setup>) {
	await user.click(await screen.findByRole("button", { name: "Add a server" }));
	await user.click(screen.getByRole("button", { name: /^Connect your own MCP server/ }));
}

/** The disclosure row of one server, opened. */
async function openRow(user: ReturnType<typeof userEvent.setup>, name: string) {
	const toggle = await screen.findByRole("button", { name: new RegExp(`^${name}`) });
	await user.click(toggle);
	expect(toggle).toHaveAttribute("aria-expanded", "true");
	const row = toggle.closest<HTMLElement>("[data-mcp-connection]");
	if (!row) throw new Error(`no row for ${name}`);
	return row;
}

beforeEach(() => {
	for (const mock of Object.values(apiMocks)) mock.mockReset();
	bridgeMocks.openExternal.mockReset().mockResolvedValue(undefined);
});

afterEach(() => {
	vi.useRealTimers();
});

describe("McpConnectionsSection", () => {
	it("lists each server with its health, tool count and projects", async () => {
		daemon(() => [
			conn({ id: "mcp_a", name: "Acme", health: "ok", tools: [search, fetchTool], projects: "all", toolsFetchedAt: 5 }),
			conn({ id: "mcp_b", name: "Docs", appKey: "do-docs", url: docsApp.url, authKind: "oauth", health: "needs_sign_in", tools: [search], projects: ["web"] }),
			conn({ id: "mcp_c", name: "Draft", url: "https://draft.test/mcp", authKind: "headers", health: "draft", projects: null }),
		]);
		renderSection();

		expect(await screen.findByText("Swarm keeps the keys and sign-ins: agents in the chosen projects get the server's tools and never see a credential.")).toBeInTheDocument();
		const acme = await screen.findByRole("button", { name: /^Acme/ });
		expect(within(acme).getByText("Working")).toBeInTheDocument();
		expect(within(acme).getByText("mcp.acme.test · 2 tools · All projects")).toBeInTheDocument();
		const docs = screen.getByRole("button", { name: /^Docs/ });
		expect(within(docs).getByText("Sign in needed")).toBeInTheDocument();
		// A known server shows its card's description.
		expect(within(docs).getByText("Search the DigitalOcean documentation. · 1 tool · 1 project")).toBeInTheDocument();
		const draft = screen.getByRole("button", { name: /^Draft/ });
		expect(within(draft).getByText("Not finished")).toBeInTheDocument();
		expect(within(draft).getByText("draft.test · 0 tools · Not active yet")).toBeInTheDocument();
	});

	it("adds a server: Check link, a key, the tools, then the projects", async () => {
		const user = userEvent.setup();
		const draft = conn({ id: "mcp_a", name: "Acme", authKind: "headers", health: "draft" });
		const keyed = { ...draft, headerNames: ["Authorization"], secrets: { "header.Authorization": "set" }, tools: [search], toolsFetchedAt: 5 };
		let list: Conn[] = [];
		daemon(() => list);
		apiMocks.post.mockImplementation(async (path: string) => {
			if (path === "/api/v1/mcp/check") {
				list = [draft];
				return { data: { ok: true, id: "mcp_a", outcome: "needs_key", connection: draft }, error: undefined };
			}
			throw new Error(`unexpected ${path}`);
		});
		apiMocks.put.mockImplementation(async (path: string, init: { body: { projects?: unknown } }) => {
			if (path === "/api/v1/mcp/connections/{id}/credentials") {
				list = [keyed];
				return { data: { ok: true, id: "mcp_a", outcome: "open", connection: keyed }, error: undefined };
			}
			if (path === "/api/v1/mcp/connections/{id}/projects") {
				const done = { ...keyed, health: "ok", projects: init.body.projects };
				list = [done];
				return { data: { ok: true, connection: done }, error: undefined };
			}
			throw new Error(`unexpected ${path}`);
		});
		renderSection();

		await startCustom(user);
		await user.type(screen.getByLabelText("Name"), "Acme");
		await user.type(screen.getByLabelText("Server URL"), "https://mcp.acme.test/mcp");
		await user.click(screen.getByRole("button", { name: "Check link" }));
		expect(apiMocks.post).toHaveBeenCalledWith("/api/v1/mcp/check", {
			body: { url: "https://mcp.acme.test/mcp", name: "Acme", appKey: undefined },
		});

		const key = await screen.findByLabelText(/^API key/);
		expect(key).toHaveAttribute("type", "password");
		expect(key).toHaveAttribute("autocomplete", "off");
		await user.type(key, "  sk-secret  ");
		await user.click(screen.getByRole("button", { name: "Save key" }));
		expect(apiMocks.put).toHaveBeenCalledWith("/api/v1/mcp/connections/{id}/credentials", {
			params: { path: { id: "mcp_a" } },
			body: { key: "sk-secret", headers: undefined },
		});

		expect(await screen.findByRole("heading", { name: "Tools found on Acme" })).toBeInTheDocument();
		expect(screen.getByText("search")).toBeInTheDocument();
		await user.click(screen.getByRole("button", { name: "Next" }));

		expect(await screen.findByRole("heading", { name: "Which projects can use this?" })).toHaveFocus();
		await user.click(screen.getByRole("radio", { name: "Only these projects" }));
		const save = screen.getByRole("button", { name: "Save projects" });
		expect(save).toBeDisabled();
		await user.click(screen.getByRole("checkbox", { name: "web" }));
		await user.click(save);
		expect(apiMocks.put).toHaveBeenCalledWith("/api/v1/mcp/connections/{id}/projects", {
			params: { path: { id: "mcp_a" } },
			body: { projects: ["web"] },
		});

		// The flow closes and the server is in the list, active.
		const row = await screen.findByRole("button", { name: /^Acme/ });
		expect(within(row).getByText("Working")).toBeInTheDocument();
		expect(within(row).getByText("mcp.acme.test · 1 tool · 1 project")).toBeInTheDocument();
		expect(screen.queryByRole("heading", { name: "Which projects can use this?" })).not.toBeInTheDocument();
		expect(screen.getByRole("button", { name: "Add a server" })).toBeInTheDocument();
	});

	it("clears a key that the server refuses, shows the reason at the field, and keeps no secret in a cache", async () => {
		const user = userEvent.setup();
		const draft = conn({ id: "mcp_a", name: "Acme", authKind: "headers", health: "draft" });
		daemon(() => [draft]);
		apiMocks.put.mockResolvedValue({
			data: { ok: false, id: "mcp_a", outcome: "error", field: "key", error: "The server refused the key.", connection: draft },
			error: undefined,
		});
		const client = renderSection();

		await user.click(within(await openRow(user, "Acme")).getByRole("button", { name: "Finish" }));
		const key = await screen.findByLabelText(/^API key/);
		await user.type(key, "sk-wrong{Enter}");

		const reason = await screen.findByText("The server refused the key.");
		expect(reason.closest("[role=alert]")).toBeInTheDocument();
		expect(key).toHaveValue("");
		expect(key).toHaveAttribute("aria-invalid", "true");
		expect(key).toHaveAttribute("aria-describedby", reason.closest("[role=alert]")?.id);
		const cached = JSON.stringify(client.getQueryCache().getAll().map((query) => [query.queryKey, query.state.data]));
		expect(cached).not.toContain("sk-wrong");
		expect(client.getMutationCache().getAll()).toHaveLength(0);
	});

	it("opens the sign-in page in the browser and reads the list until the sign-in is done", async () => {
		vi.useFakeTimers({ shouldAdvanceTime: true });
		const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
		const oauth = { issuer: "https://auth.docs.test", clientId: "swarm", scopes: [], status: "needs_sign_in", manualClient: false };
		const draft = conn({ id: "mcp_d", name: "Docs", url: "https://mcp.docs.test/mcp", authKind: "oauth", health: "needs_sign_in", oauth });
		let list: Conn[] = [];
		daemon(() => list);
		apiMocks.post.mockImplementation(async (path: string) => {
			if (path === "/api/v1/mcp/check") {
				list = [draft];
				return { data: { ok: true, id: "mcp_d", outcome: "oauth", connection: draft }, error: undefined };
			}
			if (path === "/api/v1/mcp/connections/{id}/oauth/start") {
				return { data: { ok: true, url: "https://auth.docs.test/authorize?state=abc" }, error: undefined };
			}
			throw new Error(`unexpected ${path}`);
		});
		renderSection();

		await startCustom(user);
		await user.type(screen.getByLabelText("Server URL"), "https://mcp.docs.test/mcp");
		await user.click(screen.getByRole("button", { name: "Check link" }));
		await user.click(await screen.findByRole("button", { name: "Sign in" }));

		expect(apiMocks.post).toHaveBeenCalledWith("/api/v1/mcp/connections/{id}/oauth/start", { params: { path: { id: "mcp_d" } }, body: undefined });
		await waitFor(() => expect(bridgeMocks.openExternal).toHaveBeenCalledWith("https://auth.docs.test/authorize?state=abc"));
		expect(await screen.findByText("Swarm opened the sign-in page in your browser. Finish the sign-in there, then come back to Swarm.")).toBeInTheDocument();

		// Still waiting: each poll reads the list again.
		const before = overviewReads();
		await act(() => vi.advanceTimersByTimeAsync(SIGN_IN_POLL_MS));
		await waitFor(() => expect(overviewReads()).toBeGreaterThan(before));

		// The browser finishes the sign-in; the next poll sees it and the flow goes on to the tools.
		list = [{ ...draft, health: "draft", secrets: { "oauth.access_token": "set" }, oauth: { ...oauth, status: "ok" }, tools: [search], toolsFetchedAt: 9, updatedAt: 2 }];
		await act(() => vi.advanceTimersByTimeAsync(SIGN_IN_POLL_MS));
		expect(await screen.findByRole("heading", { name: "Tools found on Docs" })).toBeInTheDocument();
		expect(screen.getByText("Signed in to Docs.")).toBeInTheDocument();

		// The poll stops.
		const settled = overviewReads();
		await act(() => vi.advanceTimersByTimeAsync(SIGN_IN_POLL_MS * 5));
		expect(overviewReads()).toBe(settled);
	});

	it("signs in again from the detail view and waits for a new sign-in", async () => {
		vi.useFakeTimers({ shouldAdvanceTime: true });
		const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
		const oauth = { issuer: "https://auth.docs.test", clientId: "swarm", scopes: [], status: "needs_sign_in", manualClient: false };
		// The old token is still saved, so only a changed row ends the wait.
		let row = conn({ id: "mcp_d", name: "Docs", authKind: "oauth", health: "needs_sign_in", projects: "all", oauth, secrets: { "oauth.access_token": "set" }, toolsFetchedAt: 3 });
		daemon(() => [row]);
		apiMocks.post.mockResolvedValue({ data: { ok: true, url: "https://auth.docs.test/authorize?state=again" }, error: undefined });
		renderSection();

		const detail = await openRow(user, "Docs");
		expect(within(detail).getByText("The sign-in through auth.docs.test is no longer valid. Sign in again.")).toBeInTheDocument();
		await user.click(within(detail).getByRole("button", { name: "Sign in again" }));
		await waitFor(() => expect(bridgeMocks.openExternal).toHaveBeenCalledWith("https://auth.docs.test/authorize?state=again"));
		expect(await within(detail).findByText(/Swarm opened the sign-in page in your browser/)).toBeInTheDocument();

		await act(() => vi.advanceTimersByTimeAsync(SIGN_IN_POLL_MS));
		expect(within(detail).getByText(/Swarm opened the sign-in page in your browser/)).toBeInTheDocument();

		row = { ...row, health: "ok", oauth: { ...oauth, status: "ok" }, updatedAt: 2 };
		await act(() => vi.advanceTimersByTimeAsync(SIGN_IN_POLL_MS));
		expect(await screen.findByText("Signed in to Docs.")).toBeInTheDocument();
		expect(within(detail).getByText("Signed in through auth.docs.test.")).toBeInTheDocument();
		expect(within(detail).queryByText(/Swarm opened the sign-in page in your browser/)).not.toBeInTheDocument();
	});

	it("reads the list again when the window gets the focus", async () => {
		daemon(() => []);
		renderSection();
		expect(await screen.findByText("No MCP servers yet.")).toBeInTheDocument();
		const before = overviewReads();
		fireEvent.focus(window);
		await waitFor(() => expect(overviewReads()).toBeGreaterThan(before));
	});

	it("reads a pasted config into drafts and shows the refused entries", async () => {
		const user = userEvent.setup();
		const keyed = conn({ id: "mcp_p", name: "acme", authKind: "headers", headerNames: ["X-Api-Key"] });
		const open = conn({ id: "mcp_q", name: "public", url: "https://public.test/mcp" });
		daemon(() => [keyed, open]);
		const text = '{"mcpServers": {"acme": {"url": "https://mcp.acme.test/mcp", "headers": {"X-Api-Key": "secret-value"}}}}';
		apiMocks.post.mockResolvedValue({
			data: {
				ok: true,
				drafts: [keyed, open],
				refused: [{ name: "local", error: "Local MCP servers that run a command are not available here. Add them with `claude mcp add` instead." }],
			},
			error: undefined,
		});
		renderSection();

		await user.click(await screen.findByRole("button", { name: "Add a server" }));
		await user.click(screen.getByRole("button", { name: "Advanced → Paste a config" }));
		const box = screen.getByRole("textbox", { name: "MCP config in JSON" });
		await user.click(box);
		await user.paste(text);
		await user.click(screen.getByRole("button", { name: "Read config" }));

		expect(apiMocks.post).toHaveBeenCalledWith("/api/v1/mcp/paste", { body: { text } });
		expect(box).toHaveValue("");
		expect(await screen.findByText(/Local MCP servers that run a command are not available here/)).toHaveTextContent(/^local: /);
		const drafts = screen.getAllByRole("listitem").filter((item) => within(item).queryByRole("button", { name: "Continue" }));
		expect(drafts).toHaveLength(2);
		expect(within(drafts[0]).getByText("acme")).toBeInTheDocument();
		expect(within(drafts[1]).getByText("public")).toBeInTheDocument();

		// A draft with header names asks for their values on the next step.
		await user.click(within(drafts[0]).getByRole("button", { name: "Continue" }));
		const name = await screen.findByLabelText("Header name");
		expect(name).toHaveValue("X-Api-Key");
		expect(name).toHaveAttribute("readonly");
		expect(screen.getByLabelText("Value for X-Api-Key")).toHaveAttribute("type", "password");
	});

	it("removes a server only after the owner confirms", async () => {
		const user = userEvent.setup();
		let list = [conn({ id: "mcp_a", name: "Acme", health: "ok", tools: [search], projects: "all", toolsFetchedAt: 5 })];
		daemon(() => list);
		apiMocks.del.mockImplementation(async () => {
			list = [];
			return { data: undefined, error: undefined, response: { ok: true, status: 204 } };
		});
		renderSection();

		const row = await openRow(user, "Acme");
		await user.click(within(row).getByRole("button", { name: "Remove" }));
		const dialog = await screen.findByRole("dialog", { name: "Remove Acme?" });
		expect(apiMocks.del).not.toHaveBeenCalled();

		await user.click(within(dialog).getByRole("button", { name: "Remove" }));
		expect(apiMocks.del).toHaveBeenCalledWith("/api/v1/mcp/connections/{id}", { params: { path: { id: "mcp_a" } } });
		await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
		expect(await screen.findByText("No MCP servers yet.")).toBeInTheDocument();
	});

	it("shows the daemon's reason when a remove fails", async () => {
		const user = userEvent.setup();
		daemon(() => [conn({ id: "mcp_a", name: "Acme", health: "ok", projects: "all", toolsFetchedAt: 5 })]);
		apiMocks.del.mockResolvedValue({
			data: undefined,
			error: { code: "MCP_STORE_BROKEN", message: "Swarm cannot read the saved connections file. Check the file, then try again." },
			response: { ok: false, status: 409 },
		});
		renderSection();

		const row = await openRow(user, "Acme");
		await user.click(within(row).getByRole("button", { name: "Remove" }));
		const dialog = await screen.findByRole("dialog", { name: "Remove Acme?" });
		await user.click(within(dialog).getByRole("button", { name: "Remove" }));
		expect(await within(dialog).findByRole("alert")).toHaveTextContent("Swarm cannot read the saved connections file. Check the file, then try again.");
	});

	it("saves the projects and a new name from the detail view", async () => {
		const user = userEvent.setup();
		let row = conn({ id: "mcp_a", name: "Acme", health: "ok", tools: [search], projects: "all", toolsFetchedAt: 5 });
		daemon(() => [row]);
		apiMocks.put.mockImplementation(async (_path: string, init: { body: { projects: unknown } }) => {
			row = { ...row, projects: init.body.projects };
			return { data: { ok: true, connection: row }, error: undefined };
		});
		apiMocks.patch.mockImplementation(async (_path: string, init: { body: { name: string } }) => {
			row = { ...row, name: init.body.name };
			return { data: { ok: true, connection: row }, error: undefined };
		});
		renderSection();

		const detail = await openRow(user, "Acme");
		const save = within(detail).getByRole("button", { name: "Save projects" });
		expect(save).toBeDisabled();
		await user.click(within(detail).getByRole("radio", { name: "Only these projects" }));
		await user.click(within(detail).getByRole("checkbox", { name: "api" }));
		await user.click(save);
		expect(apiMocks.put).toHaveBeenCalledWith("/api/v1/mcp/connections/{id}/projects", {
			params: { path: { id: "mcp_a" } },
			body: { projects: ["api"] },
		});
		expect(await within(detail).findByText("Saved.")).toBeInTheDocument();

		const name = within(detail).getByLabelText("Name");
		await user.clear(name);
		await user.type(name, "Acme docs");
		await user.click(within(detail).getByRole("button", { name: "Rename" }));
		expect(apiMocks.patch).toHaveBeenCalledWith("/api/v1/mcp/connections/{id}", { params: { path: { id: "mcp_a" } }, body: { name: "Acme docs" } });
		expect(await screen.findByRole("button", { name: /^Acme docs/ })).toBeInTheDocument();
	});

	it("says so and offers no add when this build cannot keep servers", async () => {
		daemon(() => [], { available: false });
		renderSection();
		expect(await screen.findByText("This build cannot keep MCP servers. They need the macOS keychain.")).toBeInTheDocument();
		expect(screen.queryByRole("button", { name: "Add a server" })).not.toBeInTheDocument();
	});
});
