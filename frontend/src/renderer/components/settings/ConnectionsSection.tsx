import { useQuery, useQueryClient } from "@tanstack/react-query";
import type { TFunction } from "i18next";
import { Check, ChevronDown, CircleAlert, ExternalLink, LoaderCircle } from "lucide-react";
import { useId, useRef, useState, type CSSProperties, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import type { components } from "../../../api/schema";
import { apiClient, apiErrorCode, apiErrorMessage } from "../../lib/api-client";
import { apiErrorException } from "../../lib/api-error-exception";
import { aoBridge } from "../../lib/bridge";
import { cn } from "../../lib/utils";
import { ConfirmDialog } from "../ConfirmDialog";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Label } from "../ui/label";
import { Pressable } from "../ui/pressable";
import { SettingsSection } from "./SettingsSection";

type Connection = components["schemas"]["ConnectionResponse"];
type ConnectionCheck = components["schemas"]["ConnectionCheckResponse"];
type ConnectionList = components["schemas"]["ListConnectionsResponse"];

export const connectionsQueryKey = ["connections"] as const;

async function fetchConnections(): Promise<ConnectionList> {
	const { data, error } = await apiClient.GET("/api/v1/connections");
	if (error || !data) throw apiErrorException(error);
	return data;
}

/**
 * Settings › Connections: the API keys the daemon keeps in the macOS keychain
 * for third-party services. Each connected service gives the agents one tool,
 * and the daemon attaches the key to each call, so no agent sees the key.
 * The daemon never sends a secret back: a row knows only whether a key is
 * saved, its last four characters, and the saved non-secret values.
 */
export function ConnectionsSection({ titleHidden }: { titleHidden?: boolean }) {
	const { t } = useTranslation();
	const list = useQuery({ queryKey: connectionsQueryKey, queryFn: fetchConnections });
	// Connected services lead the list as it was when the page opened. The
	// order then stays, so a row does not move away from the pointer after a
	// Connect or a Disconnect.
	const order = useRef<string[] | null>(null);
	const connections = list.data?.connections;
	if (connections && order.current === null) order.current = connectedFirst(connections);
	const rows = connections ? inOrder(connections, order.current ?? []) : [];
	const available = list.data?.available ?? false;

	return (
		<SettingsSection title={t("settings.connections")} sectionId="connections" titleHidden={titleHidden}>
			<p className="px-(--settings-row-inset) pb-1.5 text-pretty text-control leading-4 text-settings-muted">{t("settings.connections.intro")}</p>
			<div className="settings-grouped-rows flex w-full flex-col">
				{list.data && !available ? (
					<p role="status" className="settings-content-row flex items-start gap-2 text-control leading-5 text-settings-label">
						<CircleAlert className="mt-0.5 size-3.5 shrink-0 text-settings-muted" aria-hidden="true" />
						{t("settings.connections.unavailable")}
					</p>
				) : null}
				{list.isPending ? (
					<p role="status" className="settings-content-row flex items-center gap-2 text-control text-settings-muted">
						<LoaderCircle className="size-3.5 animate-spin" aria-hidden="true" />
						{t("common.loading")}
					</p>
				) : null}
				{list.isError ? (
					<p role="alert" className="settings-content-row text-xs text-error">{list.error.message || t("settings.connections.loadFailed")}</p>
				) : null}
				{rows.map((connection) => <ConnectionRow key={connection.id} connection={connection} available={available} />)}
			</div>
		</SettingsSection>
	);
}

type Pending = "connect" | "test" | "disconnect";

type Outcome =
	| { kind: "check"; action: "connect" | "test"; ok: boolean; detail: string; hadKey: boolean }
	| { kind: "error"; message: string };

/**
 * One service: a disclosure row (monogram, name, blurb, status) over its
 * form. The form stays mounted while closed, so a check that runs on a
 * closed row keeps its result.
 */
function ConnectionRow({ connection, available }: { connection: Connection; available: boolean }) {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	const baseId = useId();
	const detailsId = `${baseId}-details`;
	const [expanded, setExpanded] = useState(false);
	const [draft, setDraft] = useState<Record<string, string>>(() => savedDraft(connection));
	const [pending, setPending] = useState<Pending | null>(null);
	const [outcome, setOutcome] = useState<Outcome | null>(null);
	const [confirming, setConfirming] = useState(false);
	const [disconnectError, setDisconnectError] = useState<string | null>(null);

	const { connected, fields } = connection;
	const busy = pending !== null;
	const checking = pending === "connect" || pending === "test";
	const value = (key: string) => draft[key] ?? "";
	// A blank secret keeps the saved key, so a connected service saves when
	// anything changed; a new one needs each required secret.
	const missingSecret = !connected && fields.some((field) => field.secret && !field.optional && value(field.key).trim() === "");
	const changed = fields.some((field) => field.secret
		? value(field.key).trim() !== ""
		: value(field.key) !== (connection.values?.[field.key] ?? ""));
	const canSubmit = available && !busy && !missingSecret && (!connected || changed);

	const store = (next: Connection) => {
		queryClient.setQueryData<ConnectionList>(connectionsQueryKey, (current) => current && {
			...current,
			connections: current.connections.map((item) => (item.id === next.id ? next : item)),
		});
		// The daemon is the source of truth: read the list again.
		void queryClient.invalidateQueries({ queryKey: connectionsQueryKey });
	};
	const settle = (action: "connect" | "test", check: ConnectionCheck, hadKey: boolean) => {
		// The check carries the service's new state; without it, read the list again.
		const next = check.connection?.id === connection.id ? check.connection : null;
		if (next) store(next);
		else void queryClient.invalidateQueries({ queryKey: connectionsQueryKey });
		setOutcome({ kind: "check", action, ok: check.ok, detail: check.detail, hadKey });
		if (action === "connect" && check.ok && next) setDraft(savedDraft(next));
	};

	const submit = async (event: FormEvent) => {
		event.preventDefault();
		if (!canSubmit) return;
		const hadKey = connected;
		setPending("connect");
		setOutcome(null);
		try {
			const values = Object.fromEntries(fields.map((field) => [field.key, value(field.key).trim()]));
			const { data, error } = await apiClient.PUT("/api/v1/connections/{id}", { params: { path: { id: connection.id } }, body: { values } });
			if (error || !data) setOutcome({ kind: "error", message: errorText(error, t) });
			else settle("connect", data, hadKey);
		} catch (cause) {
			setOutcome({ kind: "error", message: errorText(cause, t) });
		} finally {
			// The key leaves the page whatever the daemon answered.
			setDraft((current) => withoutSecrets(current, connection));
			setPending(null);
		}
	};

	const test = async () => {
		setPending("test");
		setOutcome(null);
		try {
			const { data, error } = await apiClient.POST("/api/v1/connections/{id}/test", { params: { path: { id: connection.id } } });
			if (error || !data) setOutcome({ kind: "error", message: errorText(error, t) });
			else settle("test", data, true);
		} catch (cause) {
			setOutcome({ kind: "error", message: errorText(cause, t) });
		} finally {
			setPending(null);
		}
	};

	const disconnect = async () => {
		setPending("disconnect");
		setDisconnectError(null);
		try {
			const { error, response } = await apiClient.DELETE("/api/v1/connections/{id}", { params: { path: { id: connection.id } } });
			// A 204 has no body, and an error with an empty body has no error value.
			if (error !== undefined || !response.ok) {
				setDisconnectError(errorText(error, t));
				return;
			}
			const next: Connection = { ...connection, connected: false, tail: undefined, values: undefined, connectedAt: undefined };
			store(next);
			setDraft(savedDraft(next));
			setOutcome(null);
			setConfirming(false);
		} catch (cause) {
			setDisconnectError(errorText(cause, t));
		} finally {
			setPending(null);
		}
	};

	return (
		<div className="flex w-full flex-col" data-connection={connection.id}>
			<Pressable
				className="settings-row-bar group/row w-full gap-3 focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring"
				aria-expanded={expanded}
				aria-controls={detailsId}
				onClick={() => setExpanded((open) => !open)}
			>
				<Monogram name={connection.name} tint={connection.tint} />
				<span className="min-w-0 flex-1">
					<span className="block truncate text-sm leading-5 text-settings-label">{connection.name}</span>
					<span className="mt-0.5 block text-pretty text-control leading-4 text-settings-muted">{connection.blurb}</span>
				</span>
				<RowStatus connection={connection} checking={checking} />
				<ChevronDown
					className="size-4 shrink-0 text-settings-muted transition-transform duration-normal group-hover/row:text-settings-label group-aria-expanded/row:rotate-180 motion-reduce:transition-none"
					aria-hidden="true"
				/>
			</Pressable>
			{/* Mounted while closed so a check keeps running and its result stays. */}
			<div
				id={detailsId}
				inert={!expanded}
				aria-hidden={!expanded || undefined}
				className={cn(
					"overflow-hidden transition-[height,opacity] duration-normal ease-out [interpolate-size:allow-keywords] motion-reduce:transition-none",
					expanded ? "h-auto opacity-100" : "h-0 opacity-0",
				)}
			>
				<form
					className="flex flex-col gap-3 pr-(--settings-row-inset) pb-3 pl-[calc(var(--settings-row-inset)+2.5rem)]"
					aria-busy={checking || undefined}
					onSubmit={(event) => void submit(event)}
				>
					<fieldset disabled={!available || busy} className="flex min-w-0 flex-col gap-3">
						<legend className="sr-only">{connection.name}</legend>
						{fields.map((field) => {
							const inputId = `${baseId}-${field.key}`;
							const keepHint = field.secret && connected;
							return (
								<div key={field.key} className="flex flex-col gap-1.5">
									<Label htmlFor={inputId} className="gap-1.5 text-control leading-4 font-medium text-settings-label">
										{field.label}
										{field.optional ? <span className="font-normal text-settings-muted">{t("settings.connections.optional")}</span> : null}
									</Label>
									<Input
										id={inputId}
										type={field.secret ? "password" : "text"}
										autoComplete="off"
										autoCapitalize="off"
										spellCheck={false}
										placeholder={field.placeholder}
										aria-describedby={keepHint ? `${inputId}-hint` : undefined}
										className="font-mono text-control placeholder:font-sans"
										value={value(field.key)}
										onChange={(event) => {
											const next = event.target.value;
											setDraft((current) => ({ ...current, [field.key]: next }));
										}}
									/>
									{keepHint ? <p id={`${inputId}-hint`} className="text-xs leading-4 text-settings-muted">{t("settings.connections.keepKey")}</p> : null}
								</div>
							);
						})}
					</fieldset>
					<div className="flex flex-wrap items-center gap-2">
						<Button type="submit" size="sm" disabled={!canSubmit}>
							{pending === "connect" ? <LoaderCircle className="animate-spin" aria-hidden="true" /> : null}
							{t(connected ? "settings.connections.save" : "settings.connections.connect")}
						</Button>
						{connected ? (
							<Button type="button" size="sm" variant="ghost" disabled={!available || busy} onClick={() => void test()}>
								{pending === "test" ? <LoaderCircle className="animate-spin" aria-hidden="true" /> : null}
								{t("settings.connections.test")}
							</Button>
						) : null}
						{connected ? (
							<Button
								type="button"
								size="sm"
								variant="destructive-ghost"
								disabled={!available || busy}
								onClick={() => {
									setDisconnectError(null);
									setConfirming(true);
								}}
							>
								{t("settings.connections.disconnect")}
							</Button>
						) : null}
						<Button type="button" size="sm" variant="quiet" className="ml-auto" onClick={() => void aoBridge.app.openExternal(connection.keyUrl)}>
							<ExternalLink aria-hidden="true" />
							{t("settings.connections.getKey")}
						</Button>
					</div>
					{/* Always present, so a screen reader announces the check when it starts and ends. */}
					<div role="status" className="empty:hidden">
						{checking ? (
							<p className="flex items-start gap-2 text-control leading-5 text-settings-muted">
								<LoaderCircle className="mt-0.5 size-3.5 shrink-0 animate-spin" aria-hidden="true" />
								{t("settings.connections.checking", { name: connection.name })}
							</p>
						) : outcome?.kind === "check" ? (
							<div className="flex items-start gap-2 text-control leading-5">
								{outcome.ok
									? <Check className="mt-0.5 size-3.5 shrink-0 text-success" aria-hidden="true" />
									: <CircleAlert className="mt-0.5 size-3.5 shrink-0 text-error" aria-hidden="true" />}
								<div className="min-w-0">
									<p className={cn("text-pretty break-words", outcome.ok ? "text-settings-label" : "text-error")}>{sentenceCase(outcome.detail)}</p>
									{!outcome.ok && outcome.action === "connect" ? (
										<p className="text-settings-muted">{t(outcome.hadKey ? "settings.connections.keptPrevious" : "settings.connections.notSaved")}</p>
									) : null}
								</div>
							</div>
						) : null}
					</div>
					{outcome?.kind === "error" ? (
						<p role="alert" className="flex items-start gap-2 text-control leading-5 text-error">
							<CircleAlert className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
							{outcome.message}
						</p>
					) : null}
				</form>
			</div>
			{confirming ? (
				<ConfirmDialog
					open
					destructive
					title={t("settings.connections.disconnectTitle", { name: connection.name })}
					description={t("settings.connections.disconnectDescription", { tool: connection.tool })}
					confirmLabel={t("settings.connections.disconnect")}
					busy={pending === "disconnect"}
					error={disconnectError}
					onConfirm={() => void disconnect()}
					onOpenChange={(open) => {
						if (!open) setConfirming(false);
					}}
				/>
			) : null}
		</div>
	);
}

/** The row's state slot: a spinner while a check runs, else connected (with the key's tail) or not. */
function RowStatus({ connection, checking }: { connection: Connection; checking: boolean }) {
	const { t } = useTranslation();
	if (checking) {
		return (
			<span className="flex shrink-0 items-center gap-1.5 text-xs text-settings-muted">
				<LoaderCircle className="size-3.5 animate-spin" aria-hidden="true" />
				{t("settings.connections.checkingShort")}
			</span>
		);
	}
	if (!connection.connected) {
		return <span className="shrink-0 text-xs text-settings-muted">{t("settings.connections.notConnected")}</span>;
	}
	return (
		<span className="flex shrink-0 items-center gap-1.5 text-xs text-settings-label">
			<span className="size-1.5 rounded-full bg-success" aria-hidden="true" />
			<span>{t("settings.connections.connected")}</span>
			{connection.tail ? (
				<>
					<span className="font-mono text-settings-muted" aria-hidden="true">…{connection.tail}</span>
					<span className="sr-only">{t("settings.connections.keyTail", { tail: connection.tail })}</span>
				</>
			) : null}
		</span>
	);
}

const HEX_COLOR = /^#(?:[0-9a-f]{3}|[0-9a-f]{6})$/i;

/**
 * The service's initial on a faint wash of its colour. The letter mixes the
 * colour with the theme's foreground, so a near-white tint (GitHub, Vercel)
 * still reads on a light card.
 */
function Monogram({ name, tint }: { name: string; tint: string }) {
	const style: CSSProperties | undefined = HEX_COLOR.test(tint)
		? { backgroundColor: `color-mix(in oklch, ${tint} 18%, transparent)`, color: `color-mix(in oklch, ${tint} 55%, var(--foreground))` }
		: undefined;
	return (
		<span
			aria-hidden="true"
			style={style}
			className="flex size-7 shrink-0 items-center justify-center rounded-md bg-interactive-hover text-control leading-none font-semibold text-settings-muted"
		>
			{name.trim().charAt(0).toLocaleUpperCase()}
		</span>
	);
}

/** The form's start values: the saved non-secret values; a secret always starts empty. */
function savedDraft(connection: Connection): Record<string, string> {
	return Object.fromEntries(connection.fields.map((field) => [field.key, field.secret ? "" : (connection.values?.[field.key] ?? "")]));
}

function withoutSecrets(draft: Record<string, string>, connection: Connection): Record<string, string> {
	const next = { ...draft };
	for (const field of connection.fields) if (field.secret) next[field.key] = "";
	return next;
}

function connectedFirst(connections: Connection[]): string[] {
	return [...connections.filter((item) => item.connected), ...connections.filter((item) => !item.connected)].map((item) => item.id);
}

/** The connections in the frozen order; a service the order does not know goes last. */
function inOrder(connections: Connection[], order: string[]): Connection[] {
	const rank = (id: string) => {
		const index = order.indexOf(id);
		return index === -1 ? order.length : index;
	};
	return [...connections].sort((a, b) => rank(a.id) - rank(b.id));
}

/** The services' reasons start in lower case ("the API rejected the key"). */
function sentenceCase(text: string): string {
	return text ? text.charAt(0).toLocaleUpperCase() + text.slice(1) : text;
}

function errorText(error: unknown, t: TFunction): string {
	switch (apiErrorCode(error)) {
		case "CONNECTION_KEYCHAIN_DENIED":
			return t("settings.connections.error.keychainDenied");
		case "CONNECTION_BUSY":
			return t("settings.connections.error.busy");
		case "CONNECTIONS_UNAVAILABLE":
			return t("settings.connections.unavailable");
		default:
			// CONNECTION_INVALID carries a message the user can read ("API key is required").
			return apiErrorMessage(error, t("settings.connections.error.failed"));
	}
}
