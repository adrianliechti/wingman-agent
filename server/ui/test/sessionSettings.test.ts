import assert from "node:assert/strict";
import test from "node:test";
import {
	patchDraftSettings,
	settingsForDraft,
} from "../src/state/sessionSettings.ts";
import {
	EMPTY_SETTINGS,
	type SessionSettings,
	type SettingsPatch,
} from "../src/state/sessionStore.ts";

const defaults: SessionSettings = {
	...EMPTY_SETTINGS,
	models: [
		{ id: "large", name: "Large", efforts: ["low", "medium", "high", "max"] },
		{ id: "small", name: "Small", efforts: ["low", "medium"] },
	],
	model: "large",
	effort: "high",
	efforts: ["auto", "low", "medium", "high", "max"],
	mode: "agent",
	unattended: false,
};

test("draft model changes reset effort and expose the selected model's supported levels", () => {
	let patch: SettingsPatch = { effort: "max" };
	const changeModel = patchDraftSettings(settingsForDraft(defaults, patch), {
		model: "small",
	});
	patch = { ...patch, ...changeModel };
	assert.deepEqual(patch, { model: "small", effort: "auto" });
	const settings = settingsForDraft(defaults, patch);
	assert.equal(settings.effort, "auto");
	assert.deepEqual(settings.efforts, ["auto", "low", "medium"]);
	assert.equal(settings.efforts.includes("max"), false);
	assert.equal(defaults.effort, "high");
});

test("reselecting a draft model and changing mode or unattended policy preserve effort", () => {
	const current = settingsForDraft(defaults, { effort: "max" });
	for (const patch of [
		{ model: "large" },
		{ mode: "plan" },
		{ unattended: true },
	]) {
		const next = settingsForDraft(defaults, {
			effort: current.effort,
			...patchDraftSettings(current, patch),
		});
		assert.equal(next.effort, "max");
	}
});

test("an explicit model and effort patch keeps the requested effort", () => {
	const patch = patchDraftSettings(defaults, { model: "small", effort: "low" });
	assert.deepEqual(patch, { model: "small", effort: "low" });
	assert.equal(settingsForDraft(defaults, patch).effort, "low");
});

test("drafts retain defaults and handle settings without per-model metadata", () => {
	assert.deepEqual(settingsForDraft(defaults), defaults);
	assert.deepEqual(settingsForDraft(EMPTY_SETTINGS), EMPTY_SETTINGS);
	const legacy = { ...defaults, models: [{ id: "large", name: "Large" }] };
	assert.deepEqual(settingsForDraft(legacy).efforts, legacy.efforts);
	assert.equal(settingsForDraft(defaults, { model: "small" }).effort, "auto");
});
