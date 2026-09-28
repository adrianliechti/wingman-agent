import type { SessionSettings, SettingsPatch } from "./sessionStore.ts";

export function settingsForDraft(
	defaults: SessionSettings,
	patch: SettingsPatch = {},
): SessionSettings {
	const settings = { ...defaults, ...patch };
	const supported = settings.models.find(
		(m) => m.id === settings.model,
	)?.efforts;
	return {
		...settings,
		effort:
			patch.effort ??
			(settings.model === defaults.model ? defaults.effort : "auto"),
		efforts: supported ? ["auto", ...supported] : defaults.efforts,
	};
}

export function patchDraftSettings(
	current: SessionSettings,
	patch: SettingsPatch,
): SettingsPatch {
	if (
		patch.model !== undefined &&
		patch.model !== current.model &&
		patch.effort === undefined
	) {
		return { ...patch, effort: "auto" };
	}
	return patch;
}
