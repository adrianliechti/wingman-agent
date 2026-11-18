import { type RefObject, useEffect, useState } from "react";

// Sampling the level here keeps its ten updates a second out of the composer.
export function VoiceMeter({ level }: { level: RefObject<number> }) {
	const [levels, setLevels] = useState([0, 0, 0, 0]);
	useEffect(() => {
		const timer = setInterval(
			() => setLevels((previous) => [...previous.slice(1), level.current]),
			100,
		);
		return () => clearInterval(timer);
	}, [level]);
	return (
		<span className="flex h-3.5 items-center gap-0.5" aria-hidden>
			{levels.map((value, i) => (
				<span
					key={i}
					className="w-[3px] rounded-full bg-danger transition-[height] duration-100"
					style={{ height: `${25 + value * 75}%` }}
				/>
			))}
		</span>
	);
}
