import { pathToFileURL } from "node:url";
import { join } from "node:path";
import { createHash } from "node:crypto";
import { AhpClient } from "@microsoft/agent-host-protocol/client";
import { WebSocketTransport } from "@microsoft/agent-host-protocol/ws";

async function call(method: string, params: Record<string, unknown>) {
	const url = process.env.E2E_BASE_URL!.replace(/^http/, "ws") + "/ahp";
	const client = new AhpClient(await WebSocketTransport.connect(url));
	client.connect();
	try {
		await client.initialize({
			clientId: crypto.randomUUID(),
			protocolVersions: ["1.1.0"],
		});
		return await (client.request as any)(method, {
			channel: "ahp-root://",
			...params,
		});
	} finally {
		await client.shutdown();
	}
}
const uri = (path: string) =>
	pathToFileURL(join(process.env.E2E_WORKSPACE!, decodeURIComponent(path)))
		.href;
const revision = (text: string) =>
	createHash("sha256").update(text).digest("hex");
function response(value: unknown, status = 200) {
	return {
		ok: () => status < 400,
		status: () => status,
		json: async () => value,
		text: async () => JSON.stringify(value),
	};
}
export async function readSavedFile(path: string) {
	try {
		const r = await call("resourceRead", { uri: uri(path) });
		return response({
			path: decodeURIComponent(path),
			content: r.data,
			revision: revision(r.data),
		});
	} catch (error: any) {
		if (error.code === -32008) return response({}, 404);
		throw error;
	}
}
export async function writeSavedFile({ data }: any) {
	try {
		await call("resourceWrite", {
			uri: uri(data.path),
			data: data.content,
			encoding: "utf-8",
			ifMatch: data.force ? undefined : data.revision,
		});
		return response({ revision: revision(data.content) });
	} catch (error: any) {
		if (error.code === -32011) return response({}, 409);
		throw error;
	}
}
export async function createSavedFile({ data }: any) {
	await call(data.directory ? "resourceMkdir" : "resourceWrite", {
		uri: uri(data.path),
		data: data.content ?? "",
		encoding: "utf-8",
		createOnly: true,
	});
	return response({
		path: data.path,
		content: data.content ?? "",
		revision: revision(data.content ?? ""),
	});
}
