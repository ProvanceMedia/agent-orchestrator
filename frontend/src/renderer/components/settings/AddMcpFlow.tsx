// Add an MCP server: a known server card, "Connect your own MCP server" or
// Advanced → Paste a config; Check link; a key, headers or Sign in; the tools
// found; then "Which projects can use this?". Lifted from Swarm Online's
// AddMcpFlow; the calls go to the daemon instead of the gateway.
import { useQueryClient } from "@tanstack/react-query";
import { ExternalLink, LoaderCircle, Plus } from "lucide-react";
import { useEffect, useId, useState, type FormEvent, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { apiClient } from "../../lib/api-client";
import { aoBridge } from "../../lib/bridge";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Label } from "../ui/label";
import { Pressable } from "../ui/pressable";
import { ConnectionMonogram } from "./ConnectionMonogram";
import { appOf, checkFailed, hasCredential, mcpOverviewQueryKey, settle, type McpApp, type McpCheck, type McpConnection, type McpPaste } from "./mcp";
import { CredentialsForm, ErrorText, Note, PLAIN_FIELD, SignInForm, Spinner, StepTitle, ToolList, UrlLine } from "./mcpParts";
import { ProjectsControl } from "./ProjectsControl";

// Technical examples in the fields, the same in every language.
const URL_EXAMPLE = "https://example.com/mcp";
const PASTE_EXAMPLE = '{"mcpServers": {"example": {"url": "https://example.com/mcp"}}}';

type Outcome = "needs_key" | "oauth" | "manual_client";

type Step =
	| { kind: "start" }
	| { kind: "url"; app?: McpApp }
	| { kind: "paste" }
	| { kind: "credentials"; connection: McpConnection; outcome: Outcome; app?: McpApp }
	| { kind: "checking"; connection: McpConnection }
	| { kind: "tools"; connection: McpConnection }
	| { kind: "projects"; connection: McpConnection };

/**
 * Where an unfinished connection continues. The step follows the credential
 * and the tool fetch, never the tool count: a server can list no tools and
 * still be ready for the projects step.
 */
function firstStep(apps: McpApp[] | undefined, resume?: McpConnection): Step {
	if (!resume) return { kind: "start" };
	if (resume.authKind === "none") return resume.toolsFetchedAt ? { kind: "tools", connection: resume } : { kind: "checking", connection: resume };
	if (hasCredential(resume)) return { kind: "tools", connection: resume };
	return { kind: "credentials", connection: resume, outcome: resume.authKind === "oauth" ? "oauth" : "needs_key", app: appOf(apps, resume.appKey) };
}

export function AddMcpFlow({ apps, rows, resume, onDone, onCancel, onConnection }: {
	/** The known server cards. */
	apps: McpApp[];
	/** The connections as the section last read them, so a step shows the newest state (a finished sign-in, new tools). */
	rows: McpConnection[] | undefined;
	resume?: McpConnection;
	onDone: () => void;
	onCancel: () => void;
	/** The id of the connection that this flow works on, so that the section can hide its row. */
	onConnection?: (id: string | null) => void;
}) {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	// Each step that changes the daemon's rows reads the list again.
	const reload = () => void queryClient.invalidateQueries({ queryKey: mcpOverviewQueryKey });
	const [step, setStep] = useState<Step>(() => firstStep(apps, resume));
	const stepConnection = "connection" in step ? step.connection : null;
	const connectionId = stepConnection?.id ?? null;
	const live = (connectionId ? rows?.find((row) => row.id === connectionId) : undefined) ?? stepConnection;
	useEffect(() => {
		onConnection?.(connectionId);
	}, [connectionId, onConnection]);
	useEffect(() => () => onConnection?.(null), [onConnection]);
	// A sign-in finishes in the browser. When the section reads the signed-in connection, go on to its tools.
	useEffect(() => {
		if (step.kind === "credentials" && live && live.authKind === "oauth" && hasCredential(live)) setStep({ kind: "tools", connection: live });
	}, [step.kind, live]);

	const checked = (result: McpCheck, app?: McpApp): string | null => {
		if (result.connection) reload();
		if (!result.ok || !result.connection) return result.error || t("settings.mcp.checkFailed");
		if (result.outcome === "open") setStep({ kind: "tools", connection: result.connection });
		else setStep({ kind: "credentials", connection: result.connection, outcome: asOutcome(result.outcome), app });
		return null;
	};

	return (
		<div className="flex flex-col gap-3" data-mcp-flow>
			{step.kind === "start" ? (
				<StartStep
					apps={apps}
					onApp={(app) => setStep({ kind: "url", app })}
					onCustom={() => setStep({ kind: "url" })}
					onPaste={() => setStep({ kind: "paste" })}
					onCancel={onCancel}
				/>
			) : null}
			{step.kind === "url" ? <UrlStep app={step.app} onChecked={checked} onBack={() => setStep({ kind: "start" })} /> : null}
			{step.kind === "paste" ? (
				<PasteStep
					onRead={reload}
					onChecked={(result) => checked(result)}
					onHeaders={(draft) => setStep({ kind: "credentials", connection: draft, outcome: "needs_key" })}
					onBack={() => setStep({ kind: "start" })}
				/>
			) : null}
			{step.kind === "credentials" && live ? (
				<CredentialsStep
					connection={live}
					outcome={step.outcome}
					app={step.app}
					onSaved={(connection) => {
						reload();
						setStep({ kind: "tools", connection });
					}}
					onCancel={onCancel}
				/>
			) : null}
			{step.kind === "checking" ? <AutoCheck connection={step.connection} onChecked={checked} onCancel={onCancel} /> : null}
			{step.kind === "tools" && live ? (
				<ToolsStep
					connection={live}
					onRefreshed={(connection) => {
						reload();
						setStep({ kind: "tools", connection });
					}}
					onNext={() => setStep({ kind: "projects", connection: live })}
					onCancel={onCancel}
				/>
			) : null}
			{step.kind === "projects" ? (
				<ProjectsControl
					value={null}
					focus
					onCancel={onCancel}
					onSave={async (projects) => {
						const id = step.connection.id;
						const result = await settle(
							() => apiClient.PUT("/api/v1/mcp/connections/{id}/projects", { params: { path: { id } }, body: { projects } }),
							(error) => ({ ok: false, error }),
							t("settings.mcp.offline"),
						);
						if (!result.ok) return result.error || t("settings.mcp.projectsFailed");
						onDone();
						return null;
					}}
				/>
			) : null}
		</div>
	);
}

function asOutcome(outcome: string): Outcome {
	return outcome === "oauth" || outcome === "manual_client" ? outcome : "needs_key";
}

function StartStep({ apps, onApp, onCustom, onPaste, onCancel }: {
	apps: McpApp[];
	onApp: (app: McpApp) => void;
	onCustom: () => void;
	onPaste: () => void;
	onCancel: () => void;
}) {
	const { t } = useTranslation();
	return (
		<>
			<StepTitle>{t("settings.mcp.chooseServer")}</StepTitle>
			<div className="grid grid-cols-1 gap-1.5 sm:grid-cols-2">
				{apps.map((app) => (
					<Tile key={app.key} name={app.name} blurb={app.description} lead={<ConnectionMonogram name={app.name} tint={app.tint} />} onClick={() => onApp(app)} />
				))}
				<Tile
					name={t("settings.mcp.custom")}
					blurb={t("settings.mcp.customBlurb")}
					lead={(
						<span aria-hidden="true" className="flex size-7 shrink-0 items-center justify-center rounded-md bg-interactive-hover text-settings-muted">
							<Plus className="size-4" />
						</span>
					)}
					onClick={onCustom}
				/>
			</div>
			<div className="flex flex-wrap items-center gap-2">
				<Button type="button" size="sm" variant="quiet" onClick={onPaste}>{t("settings.mcp.pasteLink")}</Button>
				<Button type="button" size="sm" variant="ghost" className="ml-auto" onClick={onCancel}>{t("settings.mcp.cancel")}</Button>
			</div>
		</>
	);
}

/** A borderless source tile, as in Add Project: a faint fill at rest, a stronger fill on hover. */
function Tile({ name, blurb, lead, onClick }: { name: string; blurb: string; lead: ReactNode; onClick: () => void }) {
	return (
		<Pressable
			className="flex min-w-0 items-start gap-2.5 rounded-md bg-interactive-hover px-3 py-2.5 hover:bg-interactive-active focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring"
			onClick={onClick}
		>
			{lead}
			<span className="min-w-0">
				<span className="block truncate text-sm leading-5 text-settings-label">{name}</span>
				<span className="mt-0.5 block text-pretty text-control leading-4 text-settings-muted">{blurb}</span>
			</span>
		</Pressable>
	);
}

function UrlStep({ app, onChecked, onBack }: {
	app?: McpApp;
	onChecked: (result: McpCheck, app?: McpApp) => string | null;
	onBack: () => void;
}) {
	const { t } = useTranslation();
	const id = useId();
	const [url, setUrl] = useState(app?.url ?? "");
	const [name, setName] = useState(app?.name ?? "");
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState<{ field?: string; text: string } | null>(null);
	const check = async (event: FormEvent) => {
		event.preventDefault();
		if (busy || !url.trim()) return;
		setBusy(true);
		setError(null);
		const result = await settle(
			() => apiClient.POST("/api/v1/mcp/check", { body: { url: url.trim(), name: name.trim() || undefined, appKey: app?.key } }),
			checkFailed,
			t("settings.mcp.offline"),
		);
		setBusy(false);
		const problem = onChecked(result, app);
		if (problem) setError({ field: result.field, text: problem });
	};
	const urlError = !!error && (error.field === "url" || !error.field);
	return (
		<form className="flex flex-col gap-3" aria-busy={busy || undefined} onSubmit={(event) => void check(event)}>
			<StepTitle>{app ? app.name : t("settings.mcp.custom")}</StepTitle>
			{app?.guidance ? <Note>{app.guidance}</Note> : null}
			<div className="flex flex-col gap-1.5">
				<Label htmlFor={`${id}-name`} className="text-control leading-4 font-medium text-settings-label">{t("settings.mcp.name")}</Label>
				<Input
					id={`${id}-name`}
					{...PLAIN_FIELD}
					name="mcp-name"
					disabled={busy}
					placeholder={t("settings.mcp.namePlaceholder")}
					className="text-control"
					value={name}
					onChange={(event) => setName(event.target.value)}
				/>
			</div>
			<div className="flex flex-col gap-1.5">
				<Label htmlFor={`${id}-url`} className="text-control leading-4 font-medium text-settings-label">{t("settings.mcp.url")}</Label>
				<Input
					id={`${id}-url`}
					{...PLAIN_FIELD}
					type="url"
					name="mcp-url"
					disabled={busy}
					placeholder={URL_EXAMPLE}
					aria-invalid={urlError || undefined}
					aria-describedby={urlError ? `${id}-error` : undefined}
					className="font-mono text-control placeholder:font-sans"
					value={url}
					onChange={(event) => setUrl(event.target.value)}
				/>
				{urlError ? <ErrorText id={`${id}-error`}>{error.text}</ErrorText> : null}
			</div>
			{error && !urlError ? <ErrorText>{error.text}</ErrorText> : null}
			<div className="flex flex-wrap items-center gap-2">
				<Button type="submit" size="sm" disabled={busy || !url.trim()}>
					{busy ? <Spinner /> : null}
					{busy ? t("settings.mcp.checking") : t("settings.mcp.checkLink")}
				</Button>
				<Button type="button" size="sm" variant="ghost" disabled={busy} onClick={onBack}>{t("settings.mcp.back")}</Button>
			</div>
		</form>
	);
}

function PasteStep({ onRead, onChecked, onHeaders, onBack }: {
	/** The daemon made drafts. */
	onRead: () => void;
	onChecked: (result: McpCheck) => string | null;
	onHeaders: (draft: McpConnection) => void;
	onBack: () => void;
}) {
	const { t } = useTranslation();
	const id = useId();
	const [text, setText] = useState("");
	const [result, setResult] = useState<McpPaste | null>(null);
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState("");
	const read = async (event: FormEvent) => {
		event.preventDefault();
		if (busy || !text.trim()) return;
		setBusy(true);
		setError("");
		const pasted = text;
		// The pasted values are not kept: not in the daemon, and not in this page.
		setText("");
		const answer = await settle(
			() => apiClient.POST("/api/v1/mcp/paste", { body: { text: pasted } }),
			(failure): McpPaste => ({ ok: false, error: failure, drafts: [], refused: [] }),
			t("settings.mcp.offline"),
		);
		setBusy(false);
		if (answer.drafts.length) onRead();
		if (answer.error) setError(answer.error);
		setResult(answer);
	};
	const next = async (draft: McpConnection) => {
		if (draft.headerNames.length) {
			onHeaders(draft);
			return;
		}
		setBusy(true);
		setError("");
		const answer = await settle(() => apiClient.POST("/api/v1/mcp/check", { body: { id: draft.id } }), checkFailed, t("settings.mcp.offline"));
		setBusy(false);
		const problem = onChecked(answer);
		if (problem) setError(problem);
	};
	return (
		<form className="flex flex-col gap-3" aria-busy={busy || undefined} onSubmit={(event) => void read(event)}>
			<StepTitle>{t("settings.mcp.pasteTitle")}</StepTitle>
			<Note>{t("settings.mcp.pasteNote")}</Note>
			<textarea
				id={`${id}-paste`}
				aria-label={t("settings.mcp.pasteLabel")}
				autoComplete="off"
				autoCapitalize="off"
				autoCorrect="off"
				spellCheck={false}
				disabled={busy}
				placeholder={PASTE_EXAMPLE}
				className="settings-field-control min-h-(--size-textarea-min) min-w-0 resize-y rounded-md! py-2.5 font-mono text-control"
				value={text}
				onChange={(event) => setText(event.target.value)}
			/>
			{error ? <ErrorText>{error}</ErrorText> : null}
			{result?.refused.length ? (
				<ul className="flex flex-col gap-1">
					{result.refused.map((refusal) => (
						<li key={refusal.name} className="text-pretty text-control leading-5 text-error">
							<span className="font-mono">{refusal.name}</span>: {refusal.error}
						</li>
					))}
				</ul>
			) : null}
			{result?.drafts.length ? (
				<ul className="flex flex-col divide-y divide-border rounded-md bg-interactive-hover">
					{result.drafts.map((draft) => (
						<li key={draft.id} className="flex items-center gap-3 px-3 py-2">
							<span className="min-w-0 flex-1">
								<span className="block truncate text-sm leading-5 text-settings-label">{draft.name}</span>
								<UrlLine url={draft.url} />
							</span>
							<Button type="button" size="sm" variant="secondary" disabled={busy} onClick={() => void next(draft)}>{t("settings.mcp.continue")}</Button>
						</li>
					))}
				</ul>
			) : null}
			<div className="flex flex-wrap items-center gap-2">
				<Button type="submit" size="sm" disabled={busy || !text.trim()}>
					{busy ? <Spinner /> : null}
					{busy ? t("settings.mcp.reading") : t("settings.mcp.readConfig")}
				</Button>
				<Button type="button" size="sm" variant="ghost" disabled={busy} onClick={onBack}>{t("settings.mcp.back")}</Button>
			</div>
		</form>
	);
}

function CredentialsStep({ connection, outcome, app, onSaved, onCancel }: {
	connection: McpConnection;
	outcome: Outcome;
	app?: McpApp;
	onSaved: (connection: McpConnection) => void;
	onCancel: () => void;
}) {
	const { t } = useTranslation();
	const canSignIn = outcome === "oauth" || outcome === "manual_client";
	const [useKey, setUseKey] = useState(!canSignIn || app?.auth === "key");
	return (
		<>
			<div className="flex min-w-0 flex-col gap-0.5">
				<StepTitle>{connection.name}</StepTitle>
				<UrlLine url={connection.url} />
			</div>
			{useKey && app?.keyUrl ? (
				<Button type="button" size="sm" variant="quiet" className="w-fit" onClick={() => void aoBridge.app.openExternal(app.keyUrl!)}>
					<ExternalLink aria-hidden="true" />
					{t("settings.connections.getKey")}
				</Button>
			) : null}
			{useKey
				? <CredentialsForm connection={connection} onSaved={onSaved} submitLabel={t("settings.mcp.saveKey")} />
				: <SignInForm connection={connection} manualFirst={outcome === "manual_client"} />}
			<div className="flex flex-wrap items-center gap-2">
				{canSignIn ? (
					<Button type="button" size="sm" variant="quiet" onClick={() => setUseKey((key) => !key)}>
						{useKey ? t("settings.mcp.signInInstead") : t("settings.mcp.useKeyInstead")}
					</Button>
				) : null}
				<Button type="button" size="sm" variant="ghost" className="ml-auto" onClick={onCancel}>{t("settings.mcp.cancel")}</Button>
			</div>
		</>
	);
}

function ToolsStep({ connection, onRefreshed, onNext, onCancel }: {
	connection: McpConnection;
	onRefreshed: (connection: McpConnection) => void;
	onNext: () => void;
	onCancel: () => void;
}) {
	const { t } = useTranslation();
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState("");
	const refresh = async () => {
		setBusy(true);
		setError("");
		const result = await settle(
			() => apiClient.POST("/api/v1/mcp/connections/{id}/refresh", { params: { path: { id: connection.id } } }),
			checkFailed,
			t("settings.mcp.offline"),
		);
		setBusy(false);
		if (result.ok && result.connection) onRefreshed(result.connection);
		else setError(result.error || t("settings.mcp.toolsFailed"));
	};
	return (
		<>
			<StepTitle>{t("settings.mcp.toolsFound", { name: connection.name })}</StepTitle>
			<Note>{t("settings.mcp.toolsNote")}</Note>
			<ToolList tools={connection.tools} />
			{error ? <ErrorText>{error}</ErrorText> : null}
			<div className="flex flex-wrap items-center gap-2">
				<Button type="button" size="sm" disabled={busy} onClick={onNext}>{t("settings.mcp.next")}</Button>
				<Button type="button" size="sm" variant="ghost" disabled={busy} onClick={() => void refresh()}>
					{busy ? <Spinner /> : null}
					{busy ? t("settings.mcp.reading") : t("settings.mcp.refreshTools")}
				</Button>
				<Button type="button" size="sm" variant="ghost" className="ml-auto" disabled={busy} onClick={onCancel}>{t("settings.mcp.cancel")}</Button>
			</div>
		</>
	);
}

/** A draft with no key and no fetch yet (for example a pasted URL): run Check link for it. */
function AutoCheck({ connection, onChecked, onCancel }: {
	connection: McpConnection;
	onChecked: (result: McpCheck) => string | null;
	onCancel: () => void;
}) {
	const { t } = useTranslation();
	const [error, setError] = useState("");
	useEffect(() => {
		let live = true;
		void settle(() => apiClient.POST("/api/v1/mcp/check", { body: { id: connection.id } }), checkFailed, t("settings.mcp.offline")).then((result) => {
			if (!live) return;
			const problem = onChecked(result);
			if (problem) setError(problem);
		});
		return () => {
			live = false;
		};
		// The check runs once for this connection.
		// eslint-disable-next-line react-hooks/exhaustive-deps
	}, [connection.id]);
	return (
		<>
			<div className="flex min-w-0 flex-col gap-0.5">
				<StepTitle>{connection.name}</StepTitle>
				<UrlLine url={connection.url} />
			</div>
			{error ? <ErrorText>{error}</ErrorText> : (
				<p role="status" className="flex items-center gap-2 text-control leading-5 text-settings-muted">
					<LoaderCircle className="size-3.5 animate-spin" aria-hidden="true" />
					{t("settings.mcp.checkingLink")}
				</p>
			)}
			<div className="flex flex-wrap items-center gap-2">
				<Button type="button" size="sm" variant="ghost" className="ml-auto" onClick={onCancel}>{t("settings.mcp.cancel")}</Button>
			</div>
		</>
	);
}
