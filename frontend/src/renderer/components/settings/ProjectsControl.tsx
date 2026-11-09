// "Which projects can use this?": all projects, or the projects that the owner
// ticks. The add flow and the detail of an MCP server use it, and each
// connected key-based service in Settings › Connections uses it too.
import { useQuery } from "@tanstack/react-query";
import { CircleAlert, LoaderCircle } from "lucide-react";
import { RadioGroup } from "radix-ui";
import { useEffect, useId, useRef, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { cn } from "../../lib/utils";
import { Button } from "../ui/button";
import { Checkbox } from "../ui/checkbox";
import { Label } from "../ui/label";
import { fetchMcpOverview, mcpOverviewQueryKey, type ProjectGrant } from "./mcp";

type Mode = "all" | "some";

function picksOf(value: ProjectGrant | null): string[] {
	return Array.isArray(value) ? value : [];
}

export function ProjectsControl({
	value,
	onSave,
	onCancel,
	focus = false,
	disabled = false,
	className,
}: {
	/** The saved grant; null while the add flow has not finished. */
	value: ProjectGrant | null;
	/** Saves the grant. The result is the error text, or null. */
	onSave: (projects: ProjectGrant) => Promise<string | null>;
	onCancel?: () => void;
	/** The add flow sets this, so that the title takes the focus when the step opens. */
	focus?: boolean;
	disabled?: boolean;
	className?: string;
}) {
	const { t } = useTranslation();
	const id = useId();
	// The project list comes with the MCP overview; the query is shared with the section.
	const list = useQuery({ queryKey: mcpOverviewQueryKey, queryFn: fetchMcpOverview, select: (overview) => overview.projects });
	const [mode, setMode] = useState<Mode>(Array.isArray(value) ? "some" : "all");
	const [picked, setPicked] = useState<Set<string>>(() => new Set(picksOf(value)));
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState("");
	const [saved, setSaved] = useState(false);
	const titleRef = useRef<HTMLHeadingElement>(null);
	useEffect(() => {
		if (focus) titleRef.current?.focus();
	}, [focus]);
	// A new saved value (after a save here, or from another window) resets the choice.
	const savedKey = JSON.stringify(value ?? null);
	useEffect(() => {
		const next: ProjectGrant | null = JSON.parse(savedKey);
		setMode(Array.isArray(next) ? "some" : "all");
		setPicked(new Set(picksOf(next)));
	}, [savedKey]);

	// A saved project that is no longer in the list stays visible, so the owner can untick it.
	const options = [...new Set([...(list.data ?? []), ...picksOf(value), ...picked])].sort((a, b) => a.localeCompare(b));
	const changed = value === null
		|| (mode === "all") !== (value === "all" || !Array.isArray(value))
		|| (mode === "some" && !sameSet(picked, picksOf(value)));
	const canSave = !disabled && !busy && changed && (mode === "all" || picked.size > 0);

	const toggle = (project: string, on: boolean) => {
		setSaved(false);
		setPicked((current) => {
			const next = new Set(current);
			if (on) next.add(project);
			else next.delete(project);
			return next;
		});
	};

	const save = async () => {
		if (!canSave) return;
		setBusy(true);
		setError("");
		setSaved(false);
		let problem: string | null;
		try {
			problem = await onSave(mode === "all" ? "all" : [...picked].sort());
		} catch {
			problem = t("settings.connections.projects.failed");
		}
		setBusy(false);
		if (problem) setError(problem);
		else setSaved(true);
	};

	return (
		<div role="group" aria-labelledby={`${id}-title`} className={cn("flex flex-col gap-2", className)}>
			<h3 id={`${id}-title`} ref={titleRef} tabIndex={-1} className="text-control leading-4 font-medium text-settings-label outline-none">
				{t("settings.connections.projects.title")}
			</h3>
			<RadioGroup.Root
				aria-labelledby={`${id}-title`}
				className="flex flex-col gap-1"
				value={mode}
				disabled={disabled || busy}
				onValueChange={(next) => {
					setSaved(false);
					setMode(next === "some" ? "some" : "all");
				}}
			>
				<Choice value="all">{t("settings.connections.projects.all")}</Choice>
				<Choice value="some">{t("settings.connections.projects.some")}</Choice>
			</RadioGroup.Root>
			{mode === "some" ? (
				<div role="group" aria-label={t("settings.connections.projects.listLabel")} className="ml-6 flex max-h-48 flex-col gap-1 overflow-y-auto">
					{list.isPending ? (
						<p role="status" className="flex items-center gap-2 text-control leading-5 text-settings-muted">
							<LoaderCircle className="size-3.5 animate-spin" aria-hidden="true" />
							{t("settings.connections.projects.loading")}
						</p>
					) : null}
					{list.isError ? <p role="alert" className="text-control leading-5 text-error">{t("settings.connections.projects.loadFailed")}</p> : null}
					{list.isSuccess && options.length === 0 ? (
						<p className="text-control leading-5 text-settings-muted">{t("settings.connections.projects.empty")}</p>
					) : null}
					{options.map((project, index) => {
						// Project ids are free text, so the element id uses the position.
						const boxId = `${id}-project-${index}`;
						return (
							<div key={project} className="flex min-h-6 items-center gap-2">
								<Checkbox
									id={boxId}
									checked={picked.has(project)}
									disabled={disabled || busy}
									onCheckedChange={(on) => toggle(project, on === true)}
								/>
								<Label htmlFor={boxId} className="block min-w-0 truncate font-mono text-control leading-5 font-normal text-settings-label">{project}</Label>
							</div>
						);
					})}
				</div>
			) : null}
			<div className="flex flex-wrap items-center gap-2">
				<Button type="button" size="sm" disabled={!canSave} onClick={() => void save()}>
					{busy ? <LoaderCircle className="animate-spin" aria-hidden="true" /> : null}
					{busy ? t("settings.connections.projects.saving") : t("settings.connections.projects.save")}
				</Button>
				{onCancel ? (
					<Button type="button" size="sm" variant="ghost" disabled={busy} onClick={onCancel}>
						{t("settings.mcp.cancel")}
					</Button>
				) : null}
				<span role="status" className="text-control leading-5 text-settings-muted empty:hidden">
					{saved && !changed ? t("settings.connections.projects.saved") : null}
				</span>
			</div>
			{error ? (
				<p role="alert" className="flex items-start gap-2 text-control leading-5 text-error">
					<CircleAlert className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
					{error}
				</p>
			) : null}
		</div>
	);
}

function Choice({ value, children }: { value: Mode; children: ReactNode }) {
	return (
		<RadioGroup.Item
			value={value}
			className="group/choice flex w-fit items-center gap-2 rounded-sm py-0.5 text-left text-control leading-5 text-settings-label outline-none focus-visible:ring-2 focus-visible:ring-ring/60 disabled:cursor-not-allowed disabled:opacity-60"
		>
			<span
				aria-hidden="true"
				className="grid size-4 shrink-0 place-items-center rounded-full border border-border-strong bg-input/90 group-data-[state=checked]/choice:border-primary"
			>
				<RadioGroup.Indicator className="size-2 rounded-full bg-primary" />
			</span>
			{children}
		</RadioGroup.Item>
	);
}

function sameSet(a: Set<string>, b: string[]): boolean {
	return a.size === b.length && b.every((item) => a.has(item));
}
