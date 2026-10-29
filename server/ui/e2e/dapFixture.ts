import type { Page } from "@playwright/test";

export async function mockDebugSession(
	page: Page,
	snapshot: () => { session: any; frame?: any; output?: string },
	handle: (command: string, params: any) => any = () => undefined,
) {
	await page.routeWebSocket(/\/dap(?:\?|$)/, (socket) => {
		const server = socket.connectToServer();
		let sequence = 10000,
			root = "",
			previous = "";
		const timer = setInterval(() => {
			if (!root) return;
			const current = snapshot(),
				value = JSON.stringify(current.session);
			if (value !== previous) {
				previous = value;
				socket.send(
					JSON.stringify({
						seq: ++sequence,
						type: "event",
						event: "wingman/status",
						body: { session: current.session },
					}),
				);
			}
		}, 50);
		socket.onClose(() => {
			clearInterval(timer);
			server.close();
		});
		server.onMessage((wire) => {
			const message = JSON.parse(String(wire));
			if (message.type === "response" && message.command === "initialize")
				root = message.body._wingman.workspacePath;
			socket.send(wire);
		});
		socket.onMessage(async (wire) => {
			const request = JSON.parse(String(wire)),
				params = request.arguments ?? {};
			if (request.command === "initialize") {
				server.send(wire);
				return;
			}
			const current = snapshot();
			let body = await handle(request.command, params);
			if (body === undefined)
				switch (request.command) {
					case "wingman/status":
						body = {
							session: current.session,
							output: current.output ?? "",
							breakpoints: [],
						};
						break;
					case "threads":
						body = { threads: [{ id: 1, name: "Main" }] };
						break;
					case "stackTrace":
						body = {
							stackFrames: current.frame
								? [
										{
											...current.frame,
											source: {
												...current.frame.source,
												path: `${root}/${current.frame.source.path}`,
											},
										},
									]
								: [],
							totalFrames: current.frame ? 1 : 0,
						};
						break;
					default:
						server.send(wire);
						return;
				}
			socket.send(
				JSON.stringify({
					seq: ++sequence,
					type: "response",
					command: request.command,
					request_seq: request.seq,
					success: true,
					body,
				}),
			);
		});
	});
}
