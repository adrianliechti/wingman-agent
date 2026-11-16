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
