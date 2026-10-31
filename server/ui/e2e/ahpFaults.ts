import type { Page } from "@playwright/test";

export async function loseFirstInputReceipt(page: Page, identities: string[]) {
	let lost = false;
	await page.routeWebSocket(/\/ahp(?:\?|$)/, (socket) => {
		const server = socket.connectToServer();
		let clientId = "",
			pending: number | undefined;
		socket.onMessage((wire) => {
			const request = JSON.parse(String(wire));
			if (request.method === "initialize") clientId = request.params.clientId;
			if (
				request.method === "dispatchAction" &&
				request.params.action.type === "chat/pendingMessageSet"
			) {
				identities.push(
					`${request.params.action.id}:${request.params.clientSeq}`,
				);
				if (!lost) pending = request.params.clientSeq;
			}
			server.send(wire);
		});
		server.onMessage((wire) => {
			const message = JSON.parse(String(wire));
			if (
				!lost &&
				pending !== undefined &&
				message.method === "action" &&
				message.params.origin?.clientId === clientId &&
				message.params.origin?.clientSeq === pending
			) {
				lost = true;
				socket.close();
				server.close();
				return;
			}
			socket.send(wire);
		});
	});
}
