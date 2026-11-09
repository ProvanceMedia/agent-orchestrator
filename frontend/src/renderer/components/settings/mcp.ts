// Shared types, daemon calls and labels of Settings › Connections › MCP servers.
// The daemon keeps every credential. The renderer sends a key, a header value or
// a client secret once and reads back only names and "set".
import type { TFunction } from "i18next";
import { createContext, useContext } from "react";
import type { components } from "../../../api/schema";
import { apiClient, apiErrorMessage } from "../../lib/api-client";
import { apiErrorException } from "../../lib/api-error-exception";

export type McpConnection = components["schemas"]["MCPConnectionView"];
export type McpOverview = components["schemas"]["MCPOverviewResponse"];
export type McpApp = components["schemas"]["MCPApp"];
export type McpCheck = components["schemas"]["MCPCheckResponse"];
export type McpPaste = components["schemas"]["MCPPasteResponse"];
export type McpTool = components["schemas"]["MCPTool"];
/** "all", or the ids of the chosen projects. */
export type ProjectGrant = components["schemas"]["MCPProjects"];

export const mcpOverviewQueryKey = ["mcp", "connections"] as const;

export async function fetchMcpOverview(): Promise<McpOverview> {
	const { data, error } = await apiClient.GET("/api/v1/mcp/connections");
	if (error || !data) throw apiErrorException(error);
	return data;
}

/**
 * Sends one owner call. The daemon answers a user error with 200 and
 * `{ ok: false, error }`; a transport or envelope failure becomes the same
 * shape, so each caller reads one result.
 */
export async function settle<T>(
	send: () => Promise<{ data?: T; error?: unknown }>,
	failed: (text: string) => T,
	fallback: string,
): Promise<T> {
	try {
		const { data, error } = await send();
		if (data !== undefined && data !== null) return data;
		return failed(apiErrorMessage(error, fallback));
	} catch {
		return failed(fallback);
	}
}

export const checkFailed = (error: string): McpCheck => ({ ok: false, outcome: "error", error });

/** A header value or a sign-in token is saved for this connection. */
export function hasCredential(connection: McpConnection): boolean {
	return Object.keys(connection.secrets ?? {}).some((key) => key.startsWith("header.") || key === "oauth.access_token");
}

/** The connection holds a finished sign-in. */
export function signedIn(connection: McpConnection): boolean {
	return connection.oauth?.status === "ok" || (connection.authKind === "oauth" && connection.secrets?.["oauth.access_token"] === "set");
}

/** The app card of a known server, or undefined. */
export function appOf(apps: McpApp[] | undefined, key: string | undefined): McpApp | undefined {
	return key ? apps?.find((app) => app.key === key) : undefined;
}

export function hostOf(url: string): string {
	try {
		return new URL(url).host;
	} catch {
		return url;
	}
}

export function grantLabel(value: ProjectGrant | null | undefined, t: TFunction): string {
	if (value === null || value === undefined) return t("settings.mcp.notActive");
	if (value === "all" || !Array.isArray(value)) return t("settings.mcp.projectsAll");
	if (value.length === 0) return t("settings.mcp.projectsNone");
	return t("settings.mcp.projectsCount", { count: value.length });
}

export function healthLabel(health: string, t: TFunction): string {
	switch (health) {
		case "ok":
			return t("settings.mcp.health.ok");
		case "unreachable":
			return t("settings.mcp.health.unreachable");
		case "needs_sign_in":
			return t("settings.mcp.health.needsSignIn");
		default:
			return t("settings.mcp.health.draft");
	}
}

/**
 * A short fixed sentence for the code of the last failure. A code that Swarm
 * does not know gets a general sentence; no code text is shown.
 */
export function lastErrorText(code: string | undefined, t: TFunction): string {
	if (!code) return "";
	if (code.startsWith("network:")) {
		const detail = code.slice("network:".length);
		if (detail === "ECONNREFUSED") return t("settings.mcp.lastError.refused");
		if (detail === "ENOTFOUND" || detail === "EAI_AGAIN") return t("settings.mcp.lastError.hostNotFound");
		if (detail === "ECONNRESET") return t("settings.mcp.lastError.closed");
		if (detail === "ETIMEDOUT" || detail === "ENETUNREACH" || detail === "EHOSTUNREACH") return t("settings.mcp.lastError.unreachable");
		if (/CERT|SELF_SIGNED|SIGNATURE|ALTNAME|ISSUER/.test(detail)) return t("settings.mcp.lastError.certificate");
		return t("settings.mcp.lastError.failed");
	}
	switch (code) {
		case "network":
			return t("settings.mcp.lastError.failed");
		case "timeout":
			return t("settings.mcp.lastError.timeout");
		case "too_large":
			return t("settings.mcp.lastError.tooLarge");
		case "unauthorized":
			return t("settings.mcp.lastError.unauthorized");
		case "http_status":
			return t("settings.mcp.lastError.httpStatus");
		case "bad_reply":
			return t("settings.mcp.lastError.badReply");
		case "rpc_error":
			return t("settings.mcp.lastError.rpcError");
		case "dns_failed":
			return t("settings.mcp.lastError.hostNotFound");
		case "address_refused":
			return t("settings.mcp.lastError.addressRefused");
		case "too_many_redirects":
			return t("settings.mcp.lastError.tooManyRedirects");
		default:
			return t("settings.mcp.lastError.other");
	}
}

/**
 * The sign-in page opens in the system browser. Only an https: address opens:
 * the daemon gives the authorization address of the sign-in server.
 */
export function signInAddress(url: string | undefined): string | null {
	if (!url) return null;
	try {
		const parsed = new URL(url);
		return parsed.protocol === "https:" ? parsed.href : null;
	} catch {
		return null;
	}
}

/**
 * A sign-in that runs in the browser. The section reads the list again until
 * the connection is signed in, so each form that starts a sign-in tells the
 * section here.
 */
export type McpSignIn = {
	/** Call after the browser opened the sign-in page. */
	begin: (connection: McpConnection) => void;
	/** The connection whose sign-in Swarm waits for. */
	pendingId: string | null;
	/** The connection whose sign-in did not finish in time. */
	expiredId: string | null;
};

export const McpSignInContext = createContext<McpSignIn>({ begin: () => undefined, pendingId: null, expiredId: null });

export function useMcpSignIn(): McpSignIn {
	return useContext(McpSignInContext);
}
