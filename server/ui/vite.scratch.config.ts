import base, { proxyToServer } from "./vite.config.ts";
export default {
	...base,
	server: { proxy: proxyToServer("http://127.0.0.1:4343") },
};
