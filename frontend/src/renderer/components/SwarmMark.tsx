import { useId } from "react";

/**
 * The Swarm wasp mark. The wings are angle brackets, the abdomen is three code
 * bars, and the stinger is a short tick. The geometry and the "swarm-gold"
 * gradient come from the Swarm brand guide. Do not recolour, rotate or add to it.
 *
 * Use `small` at 22 px or less: the guide sets a thicker wing stroke there.
 */
export function SwarmMark({ className, small = false }: { className: string; small?: boolean }) {
	// One page can show the mark more than once, so each copy needs its own gradient id.
	const gradient = `swarm-gold-${useId().replace(/[^a-zA-Z0-9_-]/g, "")}`;
	const paint = `url(#${gradient})`;
	const wing = small ? 5 : 4;
	return (
		<svg aria-hidden="true" className={className} data-swarm-mark="" fill="none" viewBox="0 0 48 48">
			<defs>
				<linearGradient id={gradient} x1="0" x2="0" y1="0" y2="1">
					<stop offset="0" stopColor="#ffd257" />
					<stop offset="1" stopColor="#f29d00" />
				</linearGradient>
			</defs>
			<path d="M14 10 L6 18 L14 26" stroke={paint} strokeLinecap="round" strokeLinejoin="round" strokeWidth={wing} />
			<path d="M34 10 L42 18 L34 26" stroke={paint} strokeLinecap="round" strokeLinejoin="round" strokeWidth={wing} />
			<rect fill={paint} height="5" rx="2.5" width="14" x="17" y="14" />
			<rect fill={paint} height="5" opacity={0.65} rx="2.5" width="14" x="17" y="22" />
			<rect fill={paint} height="5" rx="2.5" width="14" x="17" y="30" />
			<path d="M24 38 L24 43" stroke="#f29d00" strokeLinecap="round" strokeWidth="3" />
		</svg>
	);
}
