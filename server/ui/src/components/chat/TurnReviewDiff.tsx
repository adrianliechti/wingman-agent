import { DiffEditor } from "@monaco-editor/react";
import type { TurnFileChange } from "../../api/turnReviews";
import { useColorScheme } from "../../hooks/useColorScheme";
import { defineWingmanThemes, wingmanThemeName } from "../../monacoThemes";
export function TurnReviewDiff({ file }: { file: TurnFileChange }) {
	const scheme = useColorScheme();
	return (
		<DiffEditor
			height="100%"
			original={file.before}
			modified={file.after}
			beforeMount={defineWingmanThemes}
			theme={wingmanThemeName(scheme)}
			options={{
				readOnly: true,
				renderSideBySide: false,
				minimap: { enabled: false },
				fontSize: 12,
				scrollBeyondLastLine: false,
				hideUnchangedRegions: { enabled: true },
			}}
		/>
	);
}
