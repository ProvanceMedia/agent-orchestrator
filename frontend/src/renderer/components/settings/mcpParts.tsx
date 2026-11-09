// The forms of the MCP add flow and detail view. A typed key, header value or
// client secret goes to the daemon once. The fields are empty again after each
// try, and no value ever comes back. These calls are plain async handlers, not
// mutations, so a secret never sits in a mutation cache.
import { CircleAlert, ExternalLink, LoaderCircle, Plus, X } from "lucide-react";
import { useEffect, useId, useRef, useState, type FormEvent, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { apiClient, apiErrorMessage } from "../../lib/api-client";
import { aoBridge } from "../../lib/bridge";
import { cn } from "../../lib/utils";
import { ConfirmDialog } from "../ConfirmDialog";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Label } from "../ui/label";
import { checkFailed, settle, signInAddress, useMcpSignIn, type McpConnection, type McpTool } from "./mcp";

/** Shared props of a password-type field: no autofill, no saving, no spell check. */
const SECRET_FIELD = { type: "password", autoComplete: "off", autoCapitalize: "off", autoCorrect: "off", spellCheck: false } as const;
export const PLAIN_FIELD = { type: "text", autoComplete: "off", autoCapitalize: "off", autoCorrect: "off", spellCheck: false } as const;

/** A step title. When the step opens, the title takes the focus, so that a keyboard or screen reader user lands on the new step. */
export function StepTitle({ children, focus = true }: { children: ReactNode; focus?: boolean }) {
	const ref = useRef<HTMLHeadingElement>(null);
	useEffect(() => {
		if (focus) ref.current?.focus();
	}, [focus]);
	return (
		<h3 ref={ref} tabIndex={-1} className="min-w-0 truncate text-sm leading-5 font-medium text-settings-label outline-none">
			{children}
		</h3>
	);
}

export function Note({ children, status }: { children: ReactNode; status?: boolean }) {
	return (
		<p role={status ? "status" : undefined} className="text-pretty text-control leading-5 text-settings-muted">
			{children}
		</p>
	);
}

export function ErrorText({ children, id }: { children: ReactNode; id?: string }) {
	return (
		<p id={id} role="alert" className="flex items-start gap-2 text-control leading-5 text-error">
			<CircleAlert className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
			<span className="min-w-0 text-pretty break-words">{children}</span>
		</p>
	);
}

/** A URL, with its secret query values masked by the daemon. */
export function UrlLine({ url }: { url: string }) {
	return <p className="min-w-0 truncate font-mono text-xs leading-4 text-settings-muted" title={url}>{url}</p>;
}

export function Spinner() {
	return <LoaderCircle className="animate-spin" aria-hidden="true" />;
}

/** Remote names and descriptions are data: React renders them as text. */
export function ToolList({ tools }: { tools: McpTool[] }) {
	const { t } = useTranslation();
	if (!tools.length) return <Note>{t("settings.mcp.noTools")}</Note>;
	return (
		<div
			role="group"
			tabIndex={0}
			aria-label={t("settings.mcp.toolsLabel")}
			className="flex max-h-56 flex-col gap-1.5 overflow-y-auto rounded-md bg-interactive-hover px-3 py-2 outline-none focus-visible:ring-2 focus-visible:ring-ring/60"
		>
			{tools.map((tool, index) => (
				<div key={`${tool.name}-${index}`} className="min-w-0">
					<p className="truncate font-mono text-xs leading-4 font-medium text-settings-label">{tool.name}</p>
					{tool.description ? (
						<p className="line-clamp-2 text-pretty text-xs leading-4 text-settings-muted" title={tool.description}>{tool.description}</p>
					) : null}
				</div>
			))}
		</div>
	);
}

/** Remove a connection after the owner confirms. */
export function RemoveMcpButton({ connection, disabled, onRemoved }: { connection: McpConnection; disabled?: boolean; onRemoved: () => void }) {
	const { t } = useTranslation();
	const [confirming, setConfirming] = useState(false);
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState<string | null>(null);
	const remove = async () => {
		setBusy(true);
		setError(null);
		try {
			const { error: failure, response } = await apiClient.DELETE("/api/v1/mcp/connections/{id}", { params: { path: { id: connection.id } } });
			// A 204 has no body, and an error with an empty body has no error value.
			if (failure !== undefined || !response.ok) {
				setError(apiErrorMessage(failure, t("settings.mcp.removeFailed")));
				return;
			}
			setConfirming(false);
			onRemoved();
		} catch {
			setError(t("settings.mcp.offline"));
		} finally {
			setBusy(false);
		}
	};
	return (
		<>
			<Button
				type="button"
				size="sm"
				variant="destructive-ghost"
				disabled={disabled}
				onClick={() => {
					setError(null);
					setConfirming(true);
				}}
			>
				{t("settings.mcp.remove")}
			</Button>
			{confirming ? (
				<ConfirmDialog
					open
					destructive
					title={t("settings.mcp.removeTitle", { name: connection.name })}
					description={t("settings.mcp.removeDescription")}
					confirmLabel={t("settings.mcp.remove")}
					busy={busy}
					error={error}
					onConfirm={() => void remove()}
					onOpenChange={(open) => {
						if (!open) setConfirming(false);
					}}
				/>
			) : null}
		</>
	);
}

type Header = { name: string; value: string };

/** An API key (sent as a Bearer key), or named headers under Advanced authentication. */
export function CredentialsForm({ connection, onSaved, submitLabel }: {
	connection: McpConnection;
	onSaved: (connection: McpConnection) => void;
	submitLabel: string;
}) {
	const { t } = useTranslation();
	const id = useId();
	// Header names from a pasted config are fixed; the owner types only their values.
	const fixed = connection.headerNames.filter((name) => name.toLowerCase() !== "authorization");
	const pasted = fixed.length > 0 && !Object.keys(connection.secrets ?? {}).some((key) => key.startsWith("header."));
	const [key, setKey] = useState("");
	const [advanced, setAdvanced] = useState(pasted);
	const [headers, setHeaders] = useState<Header[]>(() => fixed.map((name) => ({ name, value: "" })));
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState<{ field?: string; text: string } | null>(null);
	const headerError = error?.field === "header";
	const keyError = error?.field === "key";
	const filled = headers.filter((header) => header.name.trim());
	const ready = !!key.trim() || (filled.length > 0 && filled.every((header) => header.value));
	const set = (index: number, patch: Partial<Header>) =>
		setHeaders((current) => current.map((header, i) => (i === index ? { ...header, ...patch } : header)));

	const save = async (event?: FormEvent) => {
		event?.preventDefault();
		if (!ready || busy) return;
		setBusy(true);
		setError(null);
		const body = {
			key: key.trim() || undefined,
			headers: filled.length ? filled.map((header) => ({ name: header.name.trim(), value: header.value })) : undefined,
		};
		try {
			const result = await settle(
				() => apiClient.PUT("/api/v1/mcp/connections/{id}/credentials", { params: { path: { id: connection.id } }, body }),
				checkFailed,
				t("settings.mcp.offline"),
			);
			if (result.ok && result.connection) {
				onSaved(result.connection);
				return;
			}
			setError({ field: result.field, text: result.error || t("settings.mcp.keyRejected") });
			if (result.field === "header") setAdvanced(true);
		} finally {
			// The key and the header values leave the page whatever the daemon answered.
			setKey("");
			setHeaders((current) => current.map((header) => ({ ...header, value: "" })));
			setBusy(false);
		}
	};

	return (
		<form className="flex flex-col gap-3" aria-busy={busy || undefined} onSubmit={(event) => void save(event)}>
			<div className="flex flex-col gap-1.5">
				<Label htmlFor={`${id}-key`} className="gap-1.5 text-control leading-4 font-medium text-settings-label">
					{t("settings.mcp.apiKey")}
					<span className="font-normal text-settings-muted">{t("settings.mcp.apiKeyHint")}</span>
				</Label>
				<Input
					id={`${id}-key`}
					{...SECRET_FIELD}
					name="mcp-key"
					disabled={busy}
					placeholder={t("settings.mcp.keyPlaceholder")}
					aria-invalid={keyError || undefined}
					aria-describedby={keyError ? `${id}-error` : undefined}
					className="font-mono text-control placeholder:font-sans"
					value={key}
					onChange={(event) => setKey(event.target.value)}
				/>
				{keyError ? <ErrorText id={`${id}-error`}>{error?.text}</ErrorText> : null}
			</div>
			<Button type="button" variant="link" className="w-fit text-control text-settings-muted" aria-expanded={advanced} onClick={() => setAdvanced((open) => !open)}>
				{t("settings.mcp.advancedAuth")}
			</Button>
			{advanced ? (
				<div className="flex flex-col gap-2">
					{headers.map((header, index) => (
						<div key={index} className="flex items-center gap-2">
							<Input
								{...PLAIN_FIELD}
								name={`mcp-header-name-${index}`}
								aria-label={t("settings.mcp.headerName")}
								placeholder={t("settings.mcp.headerName")}
								readOnly={index < fixed.length}
								disabled={busy}
								aria-invalid={headerError || undefined}
								className="w-2/5 font-mono text-control placeholder:font-sans"
								value={header.name}
								onChange={(event) => set(index, { name: event.target.value })}
							/>
							<Input
								{...SECRET_FIELD}
								name={`mcp-header-value-${index}`}
								aria-label={header.name.trim() ? t("settings.mcp.headerValueFor", { name: header.name.trim() }) : t("settings.mcp.headerValueForBlank")}
								placeholder={t("settings.mcp.headerValue")}
								disabled={busy}
								aria-invalid={headerError || undefined}
								className="min-w-0 flex-1 font-mono text-control placeholder:font-sans"
								value={header.value}
								onChange={(event) => set(index, { value: event.target.value })}
							/>
							<Button
								type="button"
								size="icon-sm"
								variant="quiet"
								disabled={busy || index < fixed.length}
								aria-label={t("settings.mcp.removeHeader")}
								title={t("settings.mcp.removeHeader")}
								onClick={() => setHeaders((current) => current.filter((_, i) => i !== index))}
							>
								<X aria-hidden="true" />
							</Button>
						</div>
					))}
					<Button type="button" size="sm" variant="quiet" className="w-fit" disabled={busy} onClick={() => setHeaders((current) => [...current, { name: "", value: "" }])}>
						<Plus aria-hidden="true" />
						{t("settings.mcp.addHeader")}
					</Button>
					{headerError ? <ErrorText>{error?.text}</ErrorText> : null}
				</div>
			) : null}
			{error && !keyError && !(headerError && advanced) ? <ErrorText>{error.text}</ErrorText> : null}
			<div className="flex flex-wrap items-center gap-2">
				<Button type="submit" size="sm" disabled={busy || !ready}>
					{busy ? <Spinner /> : null}
					{busy ? t("settings.mcp.checking") : submitLabel}
				</Button>
			</div>
		</form>
	);
}

/**
 * The browser opened, or failed to open, the sign-in page. The section reads
 * the list again until the sign-in is done; this line says so and can open the
 * page again.
 */
export function SignInStatus({ connection, href, openFailed, onOpenAgain }: {
	connection: McpConnection;
	href: string | null;
	openFailed: boolean;
	onOpenAgain: () => void;
}) {
	const { t } = useTranslation();
	const signIn = useMcpSignIn();
	if (signIn.pendingId === connection.id) {
		return (
			<div role="status" className="flex flex-col items-start gap-2">
				<p className={cn("flex items-start gap-2 text-control leading-5", openFailed ? "text-error" : "text-settings-muted")}>
					{openFailed
						? <CircleAlert className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
						: <LoaderCircle className="mt-0.5 size-3.5 shrink-0 animate-spin" aria-hidden="true" />}
					<span className="text-pretty">{openFailed ? t("settings.mcp.browserFailed") : t("settings.mcp.signInWaiting")}</span>
				</p>
				{href ? (
					<Button type="button" size="sm" variant="quiet" onClick={onOpenAgain}>
						<ExternalLink aria-hidden="true" />
						{t("settings.mcp.signInOpenAgain")}
					</Button>
				) : null}
			</div>
		);
	}
	if (signIn.expiredId === connection.id) return <ErrorText>{t("settings.mcp.signInExpired")}</ErrorText>;
	return null;
}

/** Starts a sign-in and opens its page in the system browser. */
export function useStartSignIn(connection: McpConnection) {
	const { t } = useTranslation();
	const signIn = useMcpSignIn();
	const [href, setHref] = useState<string | null>(null);
	const [openFailed, setOpenFailed] = useState(false);
	const open = async (address: string) => {
		setHref(address);
		try {
			await aoBridge.app.openExternal(address);
			setOpenFailed(false);
		} catch {
			setOpenFailed(true);
		}
		signIn.begin(connection);
	};
	/** The result is the error text, or null when the browser got the sign-in page. */
	const start = async (client?: { clientId: string; clientSecret?: string }): Promise<{ error: string | null; manualClient: boolean }> => {
		setHref(null);
		const result = await settle(
			() => apiClient.POST("/api/v1/mcp/connections/{id}/oauth/start", { params: { path: { id: connection.id } }, body: client }),
			(error) => ({ ok: false, error }),
			t("settings.mcp.offline"),
		);
		const address = result.ok ? signInAddress(result.url) : null;
		if (address) {
			await open(address);
			return { error: null, manualClient: false };
		}
		return {
			error: result.ok ? t("settings.mcp.signInNoAddress") : result.error || t("settings.mcp.signInFailed"),
			manualClient: result.manualClient === true,
		};
	};
	const status = (
		<SignInStatus connection={connection} href={href} openFailed={openFailed} onOpenAgain={() => { if (href) void open(href); }} />
	);
	return { start, status };
}

/** Sign in with the server in the browser, with an optional client ID of the owner's own. */
export function SignInForm({ connection, manualFirst }: { connection: McpConnection; manualFirst: boolean }) {
	const { t } = useTranslation();
	const id = useId();
	const { start, status } = useStartSignIn(connection);
	const [manual, setManual] = useState(manualFirst);
	const [clientId, setClientId] = useState(connection.oauth?.manualClient ? connection.oauth.clientId : "");
	const [secret, setSecret] = useState("");
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState("");

	const submit = async (event: FormEvent) => {
		event.preventDefault();
		if (busy || (manual && !clientId.trim())) return;
		setBusy(true);
		setError("");
		const client = manual && clientId.trim() ? { clientId: clientId.trim(), clientSecret: secret || undefined } : undefined;
		try {
			const result = await start(client);
			if (result.manualClient) setManual(true);
			if (result.error) setError(result.error);
		} finally {
			// The client secret leaves the page whatever the daemon answered.
			setSecret("");
			setBusy(false);
		}
	};

	return (
		<form className="flex flex-col gap-3" aria-busy={busy || undefined} onSubmit={(event) => void submit(event)}>
			<Note>{t("settings.mcp.signInNote")}</Note>
			{manual ? (
				<>
					<div className="flex flex-col gap-1.5">
						<Label htmlFor={`${id}-client`} className="text-control leading-4 font-medium text-settings-label">{t("settings.mcp.clientId")}</Label>
						<Input
							id={`${id}-client`}
							{...PLAIN_FIELD}
							name="mcp-client-id"
							disabled={busy}
							className="font-mono text-control"
							value={clientId}
							onChange={(event) => setClientId(event.target.value)}
						/>
					</div>
					<div className="flex flex-col gap-1.5">
						<Label htmlFor={`${id}-secret`} className="gap-1.5 text-control leading-4 font-medium text-settings-label">
							{t("settings.mcp.clientSecret")}
							<span className="font-normal text-settings-muted">{t("settings.connections.optional")}</span>
						</Label>
						<Input
							id={`${id}-secret`}
							{...SECRET_FIELD}
							name="mcp-client-secret"
							disabled={busy}
							className="font-mono text-control"
							value={secret}
							onChange={(event) => setSecret(event.target.value)}
						/>
					</div>
				</>
			) : (
				<Button type="button" variant="link" className="w-fit text-control text-settings-muted" onClick={() => setManual(true)}>
					{t("settings.mcp.ownClient")}
				</Button>
			)}
			{status}
			{error ? <ErrorText>{error}</ErrorText> : null}
			<div className="flex flex-wrap items-center gap-2">
				<Button type="submit" size="sm" disabled={busy || (manual && !clientId.trim())}>
					{busy ? <Spinner /> : null}
					{busy ? t("settings.mcp.opening") : t("settings.mcp.signIn")}
				</Button>
			</div>
		</form>
	);
}
