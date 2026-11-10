import type { CSSProperties } from "react";

const HEX_COLOR = /^#(?:[0-9a-f]{3}|[0-9a-f]{6})$/i;

/**
 * The service's initial on a faint wash of its colour. The letter mixes the
 * colour with the theme's foreground, so a near-white tint (GitHub, Vercel)
 * still reads on a light card. A missing or unknown tint gets the muted wash.
 */
export function ConnectionMonogram({ name, tint }: { name: string; tint?: string }) {
	const style: CSSProperties | undefined = tint && HEX_COLOR.test(tint)
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
