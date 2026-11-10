// One MCP connection: its URL (masked when it holds a secret), how Swarm
// authenticates, the tools, Refresh tools, Sign in again, Replace key, the
// name, the project control and Remove. Lifted from Swarm Online's McpDetail.
import { useEffect, useId, useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { apiClient } from "../../lib/api-client";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Label } from "../ui/label";
import { checkFailed, hostOf, lastErrorText, settle, type McpConnection } from "./mcp";
import { CredentialsForm, ErrorText, Note, PLAIN_FIELD, RemoveMcpButton, Spinner, ToolList, UrlLine, useStartSignIn } from "./mcpParts";
import { ProjectsControl } from "./ProjectsControl";

export function McpDetail({ connection, onChanged, onRemoved }: {
	connection: McpConnection;
	/** Read the list again: the daemon is the source of truth. */
	onChanged: () => void;
	onRemoved: () => void;
}) {
	const { t } = useTranslation();
	const c = connection;
	const { start, status } = useStartSignIn(c);
	const [replacing, setReplacing] = useState(false);
	const [pending, setPending] = useState<"refresh" | "signIn" | null>(null);
	const [note, setNote] = useState("");
	const [error, setError] = useState("");
	const busy = pending !== null;

	const refresh = async () => {
		setPending("refresh");
		setNote("");
		setError("");
		const result = await settle(
			() => apiClient.POST("/api/v1/mcp/connections/{id}/refresh", { params: { path: { id: c.id } } }),
			checkFailed,
			t("settings.mcp.offline"),
		);
		setPending(null);
		if (result.ok) setNote(t("settings.mcp.toolsRead"));
		else setError(result.error || t("settings.mcp.toolsFailed"));
		onChanged();
	};

	const signIn = async () => {
		setPending("signIn");
		setNote("");
		setError("");
		try {
			const result = await start();
			if (result.error) setError(result.error);
		} finally {
			setPending(null);
		}
	};

	const auth = c.authKind === "oauth"
		? t(c.oauth?.status === "ok" ? "settings.mcp.signedInThrough" : "settings.mcp.signInEnded", { host: hostOf(c.oauth?.issuer ?? "") })
		: c.authKind === "headers"
			? t("settings.mcp.sendsHeaders", { headers: c.headerNames.join(", ") })
			: t("settings.mcp.noKey");
	const reason = (c.health === "unreachable" || c.lastError === "rpc_error") && c.lastError ? lastErrorText(c.lastError, t) : "";

	return (
		<div className="flex flex-col gap-3">
			<div className="flex min-w-0 flex-col gap-0.5">
				<UrlLine url={c.url} />
				<Note>{auth}</Note>
				{reason ? <p className="text-control leading-5 text-error">{reason}</p> : null}
			</div>
			<RenameField connection={c} onChanged={onChanged} />
			<div className="flex flex-col gap-1.5">
				<h3 className="text-control leading-4 font-medium text-settings-label">{t("settings.mcp.toolsHeading")}</h3>
				<ToolList tools={c.tools} />
			</div>
			<div className="flex flex-wrap items-center gap-2">
				<Button type="button" size="sm" variant="secondary" disabled={busy} onClick={() => void refresh()}>
					{pending === "refresh" ? <Spinner /> : null}
					{t("settings.mcp.refreshTools")}
				</Button>
				{c.authKind === "oauth" ? (
					<Button type="button" size="sm" variant="ghost" disabled={busy} onClick={() => void signIn()}>
						{pending === "signIn" ? <Spinner /> : null}
						{t("settings.mcp.signInAgain")}
					</Button>
				) : null}
				{c.authKind === "headers" ? (
					<Button type="button" size="sm" variant="ghost" disabled={busy} aria-expanded={replacing} onClick={() => setReplacing((open) => !open)}>
						{t("settings.mcp.replaceKey")}
					</Button>
				) : null}
				<span className="ml-auto">
					<RemoveMcpButton connection={c} disabled={busy} onRemoved={onRemoved} />
				</span>
			</div>
			{note ? <Note status>{note}</Note> : null}
			{status}
			{error ? <ErrorText>{error}</ErrorText> : null}
			{replacing ? (
				<CredentialsForm
					connection={c}
					submitLabel={t("settings.mcp.saveKey")}
					onSaved={() => {
						setReplacing(false);
						setNote(t("settings.mcp.keyAccepted"));
						onChanged();
					}}
				/>
			) : null}
			<ProjectsControl
				value={c.projects}
				onSave={async (projects) => {
					const result = await settle(
						() => apiClient.PUT("/api/v1/mcp/connections/{id}/projects", { params: { path: { id: c.id } }, body: { projects } }),
						(failure) => ({ ok: false, error: failure }),
						t("settings.mcp.offline"),
					);
					if (!result.ok) return result.error || t("settings.mcp.projectsFailed");
					onChanged();
					return null;
				}}
			/>
		</div>
	);
}

/** The name that agents use. A new name goes to the daemon with Rename. */
function RenameField({ connection, onChanged }: { connection: McpConnection; onChanged: () => void }) {
	const { t } = useTranslation();
	const id = useId();
	const [name, setName] = useState(connection.name);
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState("");
	// A new saved name (after Rename, or from another window) replaces the field.
	useEffect(() => setName(connection.name), [connection.name]);
	const changed = name.trim() !== "" && name.trim() !== connection.name;
	const rename = async (event: FormEvent) => {
		event.preventDefault();
		if (!changed || busy) return;
		setBusy(true);
		setError("");
		const result = await settle(
			() => apiClient.PATCH("/api/v1/mcp/connections/{id}", { params: { path: { id: connection.id } }, body: { name: name.trim() } }),
			(failure) => ({ ok: false, error: failure }),
			t("settings.mcp.offline"),
		);
		setBusy(false);
		if (!result.ok) {
			setError(result.error || t("settings.mcp.renameFailed"));
			return;
		}
		if (result.connection) setName(result.connection.name);
		onChanged();
	};
	return (
		<form className="flex flex-col gap-1.5" onSubmit={(event) => void rename(event)}>
			<Label htmlFor={`${id}-name`} className="text-control leading-4 font-medium text-settings-label">{t("settings.mcp.name")}</Label>
			<div className="flex items-center gap-2">
				<Input
					id={`${id}-name`}
					{...PLAIN_FIELD}
					name="mcp-name"
					disabled={busy}
					aria-invalid={error ? true : undefined}
					aria-describedby={error ? `${id}-error` : undefined}
					className="min-w-0 flex-1 text-control"
					value={name}
					onChange={(event) => setName(event.target.value)}
				/>
				<Button type="submit" size="sm" variant="secondary" disabled={busy || !changed}>
					{busy ? <Spinner /> : null}
					{t("settings.mcp.rename")}
				</Button>
			</div>
			{error ? <ErrorText id={`${id}-error`}>{error}</ErrorText> : null}
		</form>
	);
}
