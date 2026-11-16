import { type RefObject, useEffect, useState } from "react";

export function VoiceMeter({ levels }: { levels: number[] }) {
	return (
		<span className="flex h-3.5 items-center gap-0.5" aria-hidden>
			{levels.map((level, i) => (
				<span
					key={i}
					className="w-[3px] rounded-full bg-danger transition-[height] duration-100"
					style={{ height: `${25 + level * 75}%` }}
				/>
			))}
		</span>
	);
}
// Keep continuous conversation meter updates local to this small component.
export function ConversationMeter({ level }: { level: RefObject<number> }) {
	const [levels, setLevels] = useState([0, 0, 0, 0]);
	useEffect(() => {
		const timer = setInterval(
			() => setLevels((previous) => [...previous.slice(1), level.current]),
			100,
		);
		return () => clearInterval(timer);
	}, [level]);
	return <VoiceMeter levels={levels} />;
}
