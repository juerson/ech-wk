import { DurableObject } from "cloudflare:workers";
import { connect } from 'cloudflare:sockets';

const CONFIG = {
	TOKEN: "123456",
	CF_FALLBACK_IPS: [atob("UHJveHlJUC5DTUxpdXNzc3MubmV0"), atob("YnBiLnlvdXNlZi5pc2VnYXJvLmNvbQ=="), "84.235.245.18:34501"],
};

const SessionState = {
	INIT: "init",             // 构造后，未 CONNECT
	CONNECTING: "connecting", // connect() 中
	CONNECTED: "connected",   // TCP 已连接，正在 relay
	CLOSED: "closed"          // 已关闭
};

function parseAddress(addr) {
	if (addr[0] === "[") {
		const end = addr.indexOf("]");
		if (end === -1) throw new Error("Invalid IPv6 address");
		return { host: addr.substring(1, end), port: parseInt(addr.substring(end + 2), 10) || 443 };
	}
	const sep = addr.lastIndexOf(":");
	if (sep === -1) throw new Error("Invalid address format");
	return { host: addr.substring(0, sep), port: parseInt(addr.substring(sep + 1), 10) || 443 };
}

// 从 URL Path 提取并转换为 host:port 格式
function fallbackURLPathParse(pathname) {
	const parts = pathname.split("/").filter(Boolean);
	for (const part of parts) {
		if (!part.includes("-")) continue;
		const idx = part.lastIndexOf("-");
		const host = part.slice(0, idx);
		const port = part.slice(idx + 1);
		if (!host || !/^\d+$/.test(port)) continue;
		const portNum = Number(port);
		if (!Number.isInteger(portNum) || portNum <= 0 || portNum > 65535) continue;
		return `${host}:${portNum}`;
	}
	return null;
}

function isCFError(err) {
	const msg = err?.message?.toLowerCase() || "";
	return msg.includes("cloudflare") || msg.includes("proxy request") || msg.includes("cannot connect");
}

function safeCloseWebSocket(ws) {
	try {
		if (ws.readyState === WebSocket.READY_STATE_OPEN || ws.readyState === WebSocket.READY_STATE_CLOSING) {
			ws.close(1000, "closed");
		}
	} catch { }
}

class WebSocketSession {
	constructor(ws, pathFallback, env) {
		this.ws = ws;
		this.env = env;
		this.state = SessionState.INIT; // 会话状态
		this.TEXT_ENCODER = new TextEncoder();

		let fallbacks_ips = [];
		if (!pathFallback) {
			if (this.env && this.env.CF_FALLBACK_IPS) {
				const s = String(this.env.CF_FALLBACK_IPS).trim();
				fallbacks_ips = s.length
					? s.split(',').map(x => x.trim()).filter(Boolean)
					: CONFIG.CF_FALLBACK_IPS.slice();
			} else {
				fallbacks_ips = CONFIG.CF_FALLBACK_IPS.slice();
			}
		}
		this.fallbacks = pathFallback ? [pathFallback] : fallbacks_ips.slice();

		this.remoteSocket = null;
		this.remoteReader = null;
		this.remoteWriter = null;
		this.closed = false;

		ws.addEventListener("message", e => this.onMessage(e));
		ws.addEventListener("close", () => this.close());
		ws.addEventListener("error", () => this.close());
	}

	async onMessage(event) {
		if (this.state === SessionState.CLOSED) return;

		try {
			const data = event.data;

			if (typeof data === "string") {
				await this.handleControl(data);
			} else if (data instanceof ArrayBuffer) {
				await this.forwardBinary(new Uint8Array(data));
			}
		} catch (err) {
			this.sendError(err);
			this.close();
		}
	}

	async handleControl(text) {
		if (text.startsWith("CONNECT:")) {
			await this.handleConnect(text);
		} else if (text.startsWith("DATA:")) {
			if (this.state === SessionState.CONNECTED) {
				await this.sendRemote(text.slice(5));
			} else {
				this.sendError(new Error("Not connected yet"));
			}
		} else if (text === "CLOSE") {
			this.close();
		}
	}

	async handleConnect(text) {
		if (this.state !== SessionState.INIT) {
			throw new Error("Already connected or connecting");
		}

		this.state = SessionState.CONNECTING;

		const sep = text.indexOf("|", 8);
		if (sep < 0) throw new Error("Invalid CONNECT format");

		const target = text.slice(8, sep);
		const firstData = text.slice(sep + 1);

		const { host, port } = parseAddress(target);

		this.remoteSocket = await TcpConnector.connectWithFallback(
			host,
			port,
			this.fallbacks
		);

		this.remoteReader = this.remoteSocket.readable.getReader();
		this.remoteWriter = this.remoteSocket.writable.getWriter();

		if (firstData) {
			await this.remoteWriter.write(this.TEXT_ENCODER.encode(firstData));
		}

		this.state = SessionState.CONNECTED;
		this.ws.send("CONNECTED");

		this.pumpRemote();
	}

	async pumpRemote() {
		try {
			while (!this.closed) {
				const { done, value } = await this.remoteReader.read();
				if (done) break;
				if (this.ws.readyState !== WebSocket.READY_STATE_OPEN) break;
				if (value) this.ws.send(value);
			}
		} catch (err) {
			this.sendError(err);
		}
		this.close();
	}

	async sendRemote(text) {
		if (this.remoteWriter) {
			try {
				await this.remoteWriter.write(this.TEXT_ENCODER.encode(text));
			} catch (err) {
				this.sendError(err);
				this.close();
			}
		}
	}

	async forwardBinary(buf) {
		if (this.remoteWriter) {
			try {
				await this.remoteWriter.write(buf);
			} catch (err) {
				this.sendError(err);
				this.close();
			}
		}
	}

	sendError(err) {
		try { this.ws.send("ERROR:" + err.message); } catch { }
	}

	close() {
		if (this.state === SessionState.CLOSED) return;
		this.state = SessionState.CLOSED;

		try { this.remoteReader?.releaseLock(); } catch { }
		try { this.remoteWriter?.releaseLock(); } catch { }
		try { this.remoteSocket?.close(); } catch { }

		safeCloseWebSocket(this.ws);

		this.remoteReader = null;
		this.remoteWriter = null;
		this.remoteSocket = null;
		this.closed = true;
	}
}

class TcpConnector {
	static async connectWithFallback(host, port, fallbacks) {
		const targets = [
			{ host, port }, // 原始目标
			...fallbacks.map(fb => {
				const [h, p] = fb.split(":");
				return { host: h, port: Number(p) };
			})
		];

		for (const tgt of targets) {
			const tryHost = tgt?.host || host;
			const tryPort = Number.isInteger(tgt?.port) ? tgt.port : Number(port);
			if (!tryHost || !Number.isInteger(tryPort)) continue;

			try {
				const socket = connect({ hostname: tryHost, port: tryPort });
				await socket.opened;
				return socket;
			} catch (err) {
				if (!isCFError(err)) throw err;
			}
		}

		throw new Error("TCP connect failed");
	}
}

/**
 * The Durable Object class for handling WebSocket sessions.
 *
 * @typedef {Object} Env
 * @property {DurableObjectNamespace} ECH_SERVER_DO - The Durable Object namespace binding
 */
export class EchServerDo extends DurableObject {
	constructor(ctx, env) {
		super(ctx, env);
		this.state = ctx;
		this.env = env;
		this.token = env.TOKEN || CONFIG.TOKEN;
	}

	async fetch(request) {
		const pathname = new URL(request.url).pathname;
		const upgrade = request.headers.get("Upgrade");

		if (!upgrade || upgrade.toLowerCase() !== "websocket") {
			return new Response(pathname === '/' ? "hello world!" : "404 Not found!", { status: 200 });
		}

		if (this.token && request.headers.get('Sec-WebSocket-Protocol') !== this.token) {
			return new Response('Unauthorized', { status: 401 });
		}

		const [client, server] = Object.values(new WebSocketPair());
		server.accept();

		// 从 URL path 提取 fallback (host-port)替换为 host:port 格式
		const pathFallback = pathname.length > 3 ? fallbackURLPathParse(pathname) : null;

		new WebSocketSession(server, pathFallback, this.env);

		return new Response(null, { status: 101, webSocket: client });
	}
}

export default {
	async fetch(request, env) {
		const pathname = new URL(request.url).pathname;
		const upgrade = request.headers.get("Upgrade");
		if (!upgrade || upgrade.toLowerCase() !== "websocket") {
			return new Response(pathname === '/' ? "hello world!" : "404 Not found!", { status: 200 });
		}
		try {
			const stub = env.ECH_SERVER_DO.getByName("echserver");
			return await stub.fetch(request);
		} catch (err) {
			return new Response("Durable Object error: " + (err?.message || err), { status: 500 });
		}
	}
};
