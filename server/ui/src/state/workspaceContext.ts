import {
	createContext,
	useContext,
	useEffect,
	useSyncExternalStore,
} from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { fetchOK } from "../api/http.ts";
import { useRootAgents } from "../hooks/useAgents.ts";
import { configSettings } from "./ahpSession.ts";
import {
	EMPTY_SETTINGS,
	isDraft,
	type SessionSettings,
	type SettingsPatch,
} from "./sessionStore.ts";
import { patchDraftSettings, settingsForDraft } from "./sessionSettings.ts";
import { workspaceClient } from "./workspaceClient.ts";

export type { SettingsPatch } from "./sessionStore.ts";
type Navigation = {
	backend: string;
	selectBackend: (id: string) => void;
	drafts: Record<string, SettingsPatch>;
	setDraft: (key: string, patch: SettingsPatch) => void;
};
export const WorkspaceContext = createContext<Navigation | null>(null);
export function useWorkspace() {
	const value = useContext(WorkspaceContext);
	if (!value) throw new Error("Workspace provider missing");
	return value;
}
/** A new chat's defaults; the catalogue revision refetches them when the host discovers models. */
export function backendSettingsQuery(backend: string, catalogue = "") {
	return {
		queryKey: ["server", "backend-settings", backend, catalogue],
		queryFn: async (): Promise<SessionSettings> => {
			const ahp = workspaceClient().ahp;
			const { schema, values } = await ahp.request("resolveSessionConfig", {
				channel: "ahp-root://",
				provider: backend,
			});
			return configSettings(backend, schema.properties, values, ahp.root);
		},
	};
}
export function draftSettingsKey(key: string, draftId: string) {
	return JSON.stringify([workspaceClient().backendOf(key), draftId]);
}
export function useSessionSettings(key = "", draftId = key) {
	const queryClient = useQueryClient();
	const client = workspaceClient();
	const { backend, drafts, setDraft } = useWorkspace();
	const draft = isDraft(key);
	const owner = key ? client.backendOf(key) : backend;
	const settingsKey = JSON.stringify([owner, draftId]);
	const hasDefaults = owner === "wingman";
	const models = useRootAgents().find(
		(agent) => agent.provider === owner,
	)?.models;
	const catalogs = useQuery({
		...backendSettingsQuery(
			owner,
			models?.map((model) => model.id).join() ?? "",
		),
		enabled: draft && hasDefaults,
	});
	const sessionSettings = useSyncExternalStore(
		client.store.subscribeRender,
		() => client.store.getSnapshot()[key]?.settings ?? EMPTY_SETTINGS,
	);
	useEffect(() => client.watch(key), [client, key]);
	const settings = draft
		? settingsForDraft(
				(hasDefaults && catalogs.data) || EMPTY_SETTINGS,
				drafts[settingsKey],
			)
		: sessionSettings;
	const setSettings = async (patch: SettingsPatch) => {
		if (draft) setDraft(settingsKey, patchDraftSettings(settings, patch));
		else await client.configure(key, patch);
	};
	const retryModels = async () => {
		try {
			await fetchOK(
				`/api/v2/backends/${encodeURIComponent(owner)}/models/refresh`,
				{ method: "POST" },
			);
		} finally {
			await queryClient.invalidateQueries({
				queryKey: ["server", "backend-settings", owner],
			});
		}
	};
	return { settings, setSettings, retryModels };
}
