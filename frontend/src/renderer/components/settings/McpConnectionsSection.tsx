// Settings › Connections › MCP servers: remote MCP servers whose keys and
// sign-ins the daemon keeps. Lifted from Swarm Online's McpSection. The page
// sends a key once and reads back names, "set", health and tools.
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Check, ChevronDown, CircleAlert, LoaderCircle, Plus } from "lucide-react";
import { useCallback, useEffect, useId, useMemo, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { cn } from "../../lib/utils";
import { Button } from "../ui/button";
import { Pressable } from "../ui/pressable";
import { AddMcpFlow } from "./AddMcpFlow";
import { ConnectionMonogram } from "./ConnectionMonogram";
import {
	appOf,
	fetchMcpOverview,
	grantLabel,
	healthLabel,
	hostOf,
	mcpOverviewQueryKey,
	McpSignInContext,
	signedIn,
	type McpApp,
	type McpConnection,
	type McpSignIn,
} from "./mcp";
import { McpDetail } from "./McpDetail";
import { RemoveMcpButton, UrlLine } from "./mcpParts";
import { SettingsSection } from "./SettingsSection";

/** While a sign-in runs in the browser, the section reads the list this often. */
export const SIGN_IN_POLL_MS = 2_000;
/** The daemon forgets a sign-in after 10 minutes, so the section stops then too. */
export const SIGN_IN_LIMIT_MS = 10 * 60_000;

type PendingSignIn = {
	id: string;
	since: number;
	/** A second sign-in on a signed-in connection ends when the row changes. */
	wasSignedIn: boolean;
	updatedAt: number;
};

/** The add flow is done: agents in the chosen projects see the connection. */
function active(row: McpConnection): boolean {
	return row.projects !== null && row.projects !== undefined;
}

export function McpConnectionsSection() {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	const [pending, setPending] = useState<PendingSignIn | null>(null);
	const [expiredId, setExpiredId] = useState<string | null>(null);
	const [notice, setNotice] = useState("");
	const [adding, setAdding] = useState<{ resume?: McpConnection } | null>(null);
	const [flowId, setFlowId] = useState<string | null>(null);

	const overview = useQuery({
		queryKey: mcpOverviewQueryKey,
		queryFn: fetchMcpOverview,
		// The browser has the focus during a sign-in, so the poll runs in the background too.
		refetchInterval: pending ? SIGN_IN_POLL_MS : false,
		refetchIntervalInBackground: true,
	});
	const reload = useCallback(() => void queryClient.invalidateQueries({ queryKey: mcpOverviewQueryKey }), [queryClient]);

	// The owner comes back from the browser: read the list again.
	useEffect(() => {
		const again = () => {
			if (document.visibilityState === "visible") reload();
		};
		window.addEventListener("focus", again);
		document.addEventListener("visibilitychange", again);
		return () => {
			window.removeEventListener("focus", again);
			document.removeEventListener("visibilitychange", again);
		};
	}, [reload]);

	const rows = overview.data?.connections;
	const apps = overview.data?.apps ?? [];
	const available = overview.data?.available ?? false;
	const store = overview.data?.store;

	// The sign-in is done when the row holds a sign-in (a new one, for Sign in again).
	const readAt = overview.dataUpdatedAt;
	useEffect(() => {
		if (!pending || !rows) return;
		const row = rows.find((item) => item.id === pending.id);
		if (!row) {
			// Only a read after the start can say that the connection is gone.
			if (readAt > pending.since) setPending(null);
			return;
		}
		if (signedIn(row) && (!pending.wasSignedIn || row.updatedAt !== pending.updatedAt)) {
			setPending(null);
			setNotice(t("settings.mcp.signedIn", { name: row.name }));
		}
	}, [pending, rows, readAt, t]);

	useEffect(() => {
		if (!pending) return;
		const timer = window.setTimeout(() => {
			setPending(null);
			setExpiredId(pending.id);
		}, Math.max(0, pending.since + SIGN_IN_LIMIT_MS - Date.now()));
		return () => window.clearTimeout(timer);
	}, [pending]);

	const signIn = useMemo<McpSignIn>(() => ({
		begin: (connection) => {
			setExpiredId(null);
			setNotice("");
			setPending({ id: connection.id, since: Date.now(), wasSignedIn: signedIn(connection), updatedAt: connection.updatedAt });
		},
		pendingId: pending?.id ?? null,
		expiredId,
	}), [pending, expiredId]);

	// Another window can finish the flow's connection. Then the flow closes.
	useEffect(() => {
		if (!adding || !flowId) return;
		const row = rows?.find((item) => item.id === flowId);
		if (row && active(row)) setAdding(null);
	}, [rows, flowId, adding]);

	const finish = () => {
		setAdding(null);
		reload();
	};
	const flowCard = adding?.resume?.id ?? flowId;
	// Only the flow of the removed connection closes. Another open flow keeps its typed input.
	const removed = (id: string) => {
		if (id === flowCard) setAdding(null);
		reload();
	};
	const visible = rows?.filter((row) => !(adding && row.id === flowCard)) ?? [];
	const broken = store?.broken ?? false;
	const unreadable = (store?.unreadable ?? 0) > 0;

	return (
		<McpSignInContext.Provider value={signIn}>
			<SettingsSection
				title={t("settings.mcp.title")}
				sectionId="mcp"
				action={available && !adding ? (
					<Button type="button" size="sm" variant="ghost" onClick={() => setAdding({})}>
						<Plus aria-hidden="true" />
						{t("settings.mcp.add")}
					</Button>
				) : null}
			>
				<p className="px-(--settings-row-inset) pb-1.5 text-pretty text-control leading-4 text-settings-muted">{t("settings.mcp.intro")}</p>
				<div className="settings-grouped-rows flex w-full flex-col">
					{overview.isPending ? (
						<p role="status" className="settings-content-row flex items-center gap-2 text-control text-settings-muted">
							<LoaderCircle className="size-3.5 animate-spin" aria-hidden="true" />
							{t("common.loading")}
						</p>
					) : null}
					{overview.isError ? <Alert>{t("settings.mcp.offline")}</Alert> : null}
					{overview.data && !available ? (
						<p role="status" className="settings-content-row flex items-start gap-2 text-control leading-5 text-settings-label">
							<CircleAlert className="mt-0.5 size-3.5 shrink-0 text-settings-muted" aria-hidden="true" />
							{t("settings.mcp.unavailable")}
						</p>
					) : null}
					{broken || unreadable ? <Alert>{broken ? t("settings.mcp.storeBroken") : t("settings.mcp.storeUnreadable")}</Alert> : null}
					{notice ? (
						<p role="status" className="settings-content-row flex items-start gap-2 text-control leading-5 text-settings-label">
							<Check className="mt-0.5 size-3.5 shrink-0 text-success" aria-hidden="true" />
							{notice}
						</p>
					) : null}
					{adding ? (
						<div className="settings-content-row">
							<AddMcpFlow apps={apps} rows={rows} resume={adding.resume} onDone={finish} onCancel={finish} onConnection={setFlowId} />
						</div>
					) : null}
					{visible.map((row) => (
						<McpRow
							key={row.id}
							row={row}
							app={appOf(apps, row.appKey)}
							onFinish={() => setAdding({ resume: row })}
							onChanged={reload}
							onRemoved={() => removed(row.id)}
						/>
					))}
					{rows && rows.length === 0 && !adding && available && !broken ? (
						<p className="settings-content-row text-control leading-5 text-settings-muted">{t("settings.mcp.empty")}</p>
					) : null}
				</div>
			</SettingsSection>
		</McpSignInContext.Provider>
	);
}

function Alert({ children }: { children: ReactNode }) {
	return (
		<p role="alert" className="settings-content-row flex items-start gap-2 text-control leading-5 text-error">
			<CircleAlert className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
			{children}
		</p>
	);
}

/**
 * One server: a disclosure row (monogram, name, what it is, tools, projects,
 * health) over its detail. An unfinished server offers Finish and Remove.
 */
function McpRow({ row, app, onFinish, onChanged, onRemoved }: {
	row: McpConnection;
	app?: McpApp;
	onFinish: () => void;
	onChanged: () => void;
	onRemoved: () => void;
}) {
	const { t } = useTranslation();
	const baseId = useId();
	const detailsId = `${baseId}-details`;
	const [expanded, setExpanded] = useState(false);
	const draft = !active(row);
	const meta = [app?.description ?? hostOf(row.url), t("settings.mcp.tools", { count: row.tools.length }), grantLabel(row.projects, t)].join(" · ");

	return (
		<div className="flex w-full flex-col" data-mcp-connection={row.id}>
			<Pressable
				className="settings-row-bar group/row w-full gap-3 focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring"
				aria-expanded={expanded}
				aria-controls={detailsId}
				onClick={() => setExpanded((open) => !open)}
			>
				<ConnectionMonogram name={row.name} tint={app?.tint} />
				<span className="min-w-0 flex-1">
					<span className="block truncate text-sm leading-5 text-settings-label">{row.name}</span>
					<span className="mt-0.5 block text-pretty text-control leading-4 text-settings-muted">{meta}</span>
				</span>
				<HealthStatus health={row.health} />
				<ChevronDown
					className="size-4 shrink-0 text-settings-muted transition-transform duration-normal group-hover/row:text-settings-label group-aria-expanded/row:rotate-180 motion-reduce:transition-none"
					aria-hidden="true"
				/>
			</Pressable>
			{/* Mounted while closed, so a refresh keeps running and its result stays. */}
			<div
				id={detailsId}
				inert={!expanded}
				aria-hidden={!expanded || undefined}
				className={cn(
					"overflow-hidden transition-[height,opacity] duration-normal ease-out [interpolate-size:allow-keywords] motion-reduce:transition-none",
					expanded ? "h-auto opacity-100" : "h-0 opacity-0",
				)}
			>
				<div className="flex flex-col gap-3 pr-(--settings-row-inset) pb-3 pl-[calc(var(--settings-row-inset)+2.5rem)]">
					{draft ? (
						<>
							<UrlLine url={row.url} />
							<div className="flex flex-wrap items-center gap-2">
								<Button type="button" size="sm" onClick={onFinish}>{t("settings.mcp.finish")}</Button>
								<span className="ml-auto">
									<RemoveMcpButton connection={row} onRemoved={onRemoved} />
								</span>
							</div>
						</>
					) : (
						<McpDetail connection={row} onChanged={onChanged} onRemoved={onRemoved} />
					)}
				</div>
			</div>
		</div>
	);
}

/** The row's state slot: a dot in the health colour, and the health in words. */
function HealthStatus({ health }: { health: string }) {
	const { t } = useTranslation();
	const dot = health === "ok" ? "bg-success" : health === "needs_sign_in" ? "bg-warning" : health === "unreachable" ? "bg-error" : null;
	return (
		<span className={cn("flex shrink-0 items-center gap-1.5 text-xs", dot ? "text-settings-label" : "text-settings-muted")}>
			{dot ? <span className={cn("size-1.5 rounded-full", dot)} aria-hidden="true" /> : null}
			<span>{healthLabel(health, t)}</span>
		</span>
	);
}
