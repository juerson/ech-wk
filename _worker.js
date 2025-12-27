import { connect } from "cloudflare:sockets";

const DEFAULTS = {
	TOKEN: "", // 身份令牌
	FALLBACK_IPS: "ProxyIP.CMLiussss.net,43.132.244.52:21415",
	CONNECT_TIMEOUT_MS: 5000, // 5 seconds
	READ_TIMEOUT_MS: 180000, // 3 minutes
	MAX_SESSIONS: 100, // Maximum concurrent sessions
	LOG_LEVEL: 1, // 1=ERROR, 2=WARN, 3=INFO, 4=DEBUG
	ALLOWED_HOSTS: "",
	ALLOW_ORIGIN: "*",
};
const CMD = {
	CONNECT: "CONNECT:",
	DATA: "DATA:",
	CLOSE: "CLOSE",
	ERROR: "ERROR:",
	PING: "PING",
	PONG: "PONG",
};
const LOG_LEVELS = { ERROR: 1, WARN: 2, INFO: 3, DEBUG: 4 };
const WS_STATE = { CONNECTING: 0, OPEN: 1, CLOSING: 2, CLOSED: 3 };
const textEncoder = new TextEncoder();

class Logger {
	constructor(level = LOG_LEVELS.INFO) { this.level = level; }
	_log(level, ...msg) {
		if (level > this.level) return;
		if (level <= LOG_LEVELS.ERROR) console.error(...msg);
		else if (level <= LOG_LEVELS.WARN) console.warn(...msg);
		else console.log(...msg);
	}
	error(...m) { this._log(LOG_LEVELS.ERROR, ...m); }
	warn(...m) { this._log(LOG_LEVELS.WARN, ...m); }
	info(...m) { this._log(LOG_LEVELS.INFO, ...m); }
	debug(...m) { this._log(LOG_LEVELS.DEBUG, ...m); }
}
let globalLogger = new Logger(DEFAULTS.LOG_LEVEL);

class Config {
	constructor(request, env) {
		const url = new URL(request.url);
		const pathIps = Config._parsePath(url);
		const fallbackIps = (env.FALLBACK_IPS ?? DEFAULTS.FALLBACK_IPS)
			.split(",").map(s => s.trim()).filter(Boolean);
		this.FALLBACK_IPS = pathIps.length ? pathIps : fallbackIps;
		this.CONNECT_TIMEOUT_MS = Number(env.CONNECT_TIMEOUT_MS) || DEFAULTS.CONNECT_TIMEOUT_MS;
		this.READ_TIMEOUT_MS = Number(env.READ_TIMEOUT_MS) || DEFAULTS.READ_TIMEOUT_MS;
		this.MAX_SESSIONS = Number(env.MAX_SESSIONS) || DEFAULTS.MAX_SESSIONS;
		this.LOG_LEVEL = Number(env.LOG_LEVEL) || DEFAULTS.LOG_LEVEL;
		this.ALLOWED_HOSTS = new Set((env.ALLOWED_HOSTS ?? DEFAULTS.ALLOWED_HOSTS)
			.split(",").map(s => s.trim()).filter(Boolean));
		this.TOKEN = env.TOKEN ?? DEFAULTS.TOKEN;
		this.ALLOW_ORIGIN = env.ALLOW_ORIGIN ?? DEFAULTS.ALLOW_ORIGIN;
	}
	// Parse path segments for fallback IPs, e.g. /domain,ip-port
	static _parsePath(url) {
		const segments = url.pathname.split("/").filter(Boolean);
		if (!segments.length) return [];
		const segment = segments[segments.length - 1];
		if (!segment) return [];
		return segment.split(",")
			.map(s => s.trim())
			.filter(Boolean)
			.map(s => s.includes("-") ? s.replace(/-/g, ":") : s);
	}
}

class SessionPool {
	constructor(max) { this.max = max; this.current = 0; }
	acquire() {
		if (this.current >= this.max) return false;
		this.current++;
		return true;
	}
	release() { this.current = Math.max(this.current - 1, 0); }
}

class Message {
	static async _blobToArrayBuffer(blob) {
		if (blob.arrayBuffer) return await blob.arrayBuffer();
		return new Response(blob).arrayBuffer();
	}

	// Parse an incoming WebSocket message event (string, ArrayBuffer, TypedArray, Blob)
	static async parse(ev) {
		const data = ev.data;
		if (typeof data === "string") {
			if (data.startsWith(CMD.CONNECT)) return { type: "CONNECT", payload: data.slice(CMD.CONNECT.length) };
			if (data.startsWith(CMD.DATA)) return { type: "DATA", payload: data.slice(CMD.DATA.length) };
			if (data.startsWith(CMD.ERROR)) return { type: "ERROR", payload: data.slice(CMD.ERROR.length) };
			if (data === CMD.PING) return { type: "PING", payload: null };
			if (data === CMD.PONG) return { type: "PONG", payload: null };
			if (data === CMD.CLOSE) return { type: "CLOSE", payload: null };
			return { type: "RAW", payload: data };
		}

		// Binary types
		if (data instanceof ArrayBuffer) return { type: "DATA", payload: data };
		if (ArrayBuffer.isView(data)) {
			const view = data;
			// Avoid copying: return either the ArrayBuffer (when view covers whole buffer) or a Uint8Array view
			if (view.byteOffset === 0 && view.byteLength === view.buffer.byteLength) {
				return { type: "DATA", payload: view.buffer };
			}
			// Return a Uint8Array view (no buffer copy)
			return { type: "DATA", payload: new Uint8Array(view.buffer, view.byteOffset, view.byteLength) };
		}

		// Blob or other
		try {
			const ab = await Message._blobToArrayBuffer(data);
			return { type: "DATA", payload: ab };
		} catch (e) {
			// fallback: pass original
			return { type: "DATA", payload: data };
		}
	}

	// Encode payload for sending over WebSocket when type === "DATA"
	// Returns either ArrayBuffer or ArrayBufferView (Uint8Array) or string for non-DATA types
	static encode(type, payload) {
		if (type === "DATA") {
			if (payload instanceof ArrayBuffer) return payload;
			if (ArrayBuffer.isView(payload)) {
				const view = payload;
				if (view.byteOffset === 0 && view.byteLength === view.buffer.byteLength) {
					// full buffer: return raw ArrayBuffer
					return view.buffer;
				}
				// return a Uint8Array view (no copy of backing buffer)
				return new Uint8Array(view.buffer, view.byteOffset, view.byteLength);
			}
			// strings / other -> encode to Uint8Array
			return textEncoder.encode(String(payload));
		}
		if (payload == null) return String(type);
		return `${type}:${payload}`;
	}
}

function safeCloseWebSocket(ws) {
	try {
		if (!ws) return;
		const state = ws.readyState;
		if (typeof state === "number" && state !== WS_STATE.CLOSED) {
			try { ws.close(1000, "normal"); } catch (_) { /* ignore */ }
		}
	} catch (_) { /* ignore */ }
}

async function waitForBackpressure(ws, limit = 1 << 20) {
	if (!ws || typeof ws.bufferedAmount !== "number") return;
	let delay = 8;
	while (ws.bufferedAmount > limit) {
		await new Promise(r => setTimeout(r, delay));
		delay = Math.min(200, Math.round(delay * 1.5));
	}
}

async function writeToWritable(writer, data) {
	if (!writer) throw new Error("No writer available");
	let arrayData;
	if (data instanceof ArrayBuffer) arrayData = new Uint8Array(data);
	else if (ArrayBuffer.isView(data)) arrayData = new Uint8Array(data.buffer, data.byteOffset, data.byteLength);
	else arrayData = textEncoder.encode(String(data));
	await writer.write(arrayData);
}

function sendError(ws, err) {
	try {
		const payload = JSON.stringify({ msg: err?.message ?? String(err), name: err?.name ?? "Error" });
		ws.send(Message.encode("ERROR", payload));
	} catch (_) { /* ignore */ }
}

class TcpWsSession {
	constructor(ws, cfg, log, releaseFn) {
		this.ws = ws;
		this.cfg = cfg;
		this.log = log;
		this.release = releaseFn;
		this.remote = null; // { socket, reader, writer }
		this.readTimeout = null;
		this.closed = false;
		this._onWsMessage = this._onWsMessage.bind(this);
		this._onWsClose = this._onWsClose.bind(this);
		this._onWsError = this._onWsError.bind(this);
	}

	start() {
		this.ws.addEventListener("message", this._onWsMessage);
		this.ws.addEventListener("close", this._onWsClose);
		this.ws.addEventListener("error", this._onWsError);
	}

	async _dispose() {
		if (this.closed) return;
		this.closed = true;
		if (this.readTimeout) clearTimeout(this.readTimeout);
		try {
			this.ws.removeEventListener("message", this._onWsMessage);
			this.ws.removeEventListener("close", this._onWsClose);
			this.ws.removeEventListener("error", this._onWsError);
		} catch (_) { /* ignore */ }
		await this._closeRemote();
		safeCloseWebSocket(this.ws);
		try { this.release(); } catch (_) { /* ignore */ }
		this.log.debug("🧹 Session disposed");
	}

	async _closeRemote() {
		try { await this.remote?.writer?.close(); } catch (_) { /* ignore */ }
		try { await this.remote?.reader?.cancel(); } catch (_) { /* ignore */ }
		try { this.remote?.socket?.close?.(); } catch (_) { /* ignore */ }
		this.remote = null;
	}

	_resetReadTimeout() {
		if (this.readTimeout) clearTimeout(this.readTimeout);
		this.readTimeout = setTimeout(() => {
			this.log.warn("⏱️ Read timeout – closing session");
			this._dispose();
		}, this.cfg.READ_TIMEOUT_MS);
	}

	async _pumpRemote() {
		const reader = this.remote?.reader;
		if (!reader) return;
		try {
			while (!this.closed) {
				const { done, value } = await reader.read();
				if (done || this.ws.readyState !== WS_STATE.OPEN) break;
				await waitForBackpressure(this.ws);
				try {
					// Send data while avoiding copies:
					if (value instanceof Uint8Array) {
						// send the Uint8Array view directly (no copy)
						this.ws.send(value);
					} else if (value instanceof ArrayBuffer) {
						this.ws.send(value);
					} else if (ArrayBuffer.isView(value)) {
						const view = value;
						// send a Uint8Array view (no copy)
						const u8 = view instanceof Uint8Array ? view : new Uint8Array(view.buffer, view.byteOffset, view.byteLength);
						this.ws.send(u8);
					} else {
						// fallback: construct a Uint8Array from value
						const arr = new Uint8Array(value);
						this.ws.send(arr);
					}
				} catch (e) {
					this.log.error("🚨 ws.send error in pumpRemote:", e);
					throw e;
				}
				this._resetReadTimeout();
			}
		} catch (e) {
			if (e?.name !== "AbortError") {
				this.log.error("🚨 pumpRemote error:", e);
				sendError(this.ws, e);
			}
		} finally {
			await this._dispose();
		}
	}

	async _connectRemote(target, firstPayload) {
		const { host: rawHost, port: rawPort } = parseAddress(target);
		const port = Number(rawPort);
		if (!Number.isInteger(port) || port <= 0 || port > 65535) throw new Error(`Invalid port: ${rawPort}`);
		if (this.cfg.ALLOWED_HOSTS.size && !this.cfg.ALLOWED_HOSTS.has(rawHost)) throw new Error(`Host ${rawHost} is not allowed`);

		const attempts = isIpAddress(rawHost)
			? [{ host: rawHost, port }]
			: [{ host: rawHost, port }, ...this._fallbackAttempts(port)];

		for (let i = 0; i < attempts.length; i++) {
			const { host, port } = attempts[i];
			this.log.info(`🔌 Attempt ${i + 1}/${attempts.length}: ${host}:${port}`);
			try {
				const socket = connect({ hostname: host, port });
				const openedPromise = socket.opened;
				await Promise.race([
					openedPromise,
					new Promise((_, rej) => setTimeout(() => rej(new Error("connect timeout")), this.cfg.CONNECT_TIMEOUT_MS))
				]);
				this.remote = {
					socket,
					writer: socket.writable.getWriter(),
					reader: socket.readable.getReader(),
				};
				if (firstPayload) {
					await writeToWritable(this.remote.writer, firstPayload);
				}
				try { this.ws.send("CONNECTED"); } catch (_) { /* ignore */ }
				this._resetReadTimeout();
				void this._pumpRemote();
				this.log.info(`Connected to ${host}:${port}`);
				return;
			} catch (e) {
				this.log.warn("Connect attempt failed:", e?.message ?? e);
				try { e?.socket?.close?.(); } catch (_) { /* ignore */ }
				await this._closeRemote();
				if (!isCFError(e) || i === attempts.length - 1) throw e;
				this.log.warn("Transient CF error, trying next fallback:", e?.message ?? e);
			}
		}
	}

	_fallbackAttempts(defaultPort) {
		return this.cfg.FALLBACK_IPS.map(item =>
			item.includes(":") ? parseAddress(item) : { host: item, port: defaultPort }
		);
	}

	async _onWsMessage(ev) {
		if (this.closed) return;
		let parsed;
		try {
			parsed = await Message.parse(ev);
		} catch (e) {
			this.log.error("Failed to parse message:", e);
			sendError(this.ws, e);
			return;
		}
		const { type, payload } = parsed;

		try {
			switch (type) {
				case "PING":
					this.ws.send(CMD.PONG);
					break;
				case "PONG":
					break;
				case "CLOSE":
					await this._dispose();
					break;
				case "CONNECT": {
					const sepIndex = payload.indexOf("|");
					const target = sepIndex >= 0 ? payload.slice(0, sepIndex) : payload;
					const firstPayload = sepIndex >= 0 ? payload.slice(sepIndex + 1) : null;
					await this._connectRemote(target, firstPayload);
					break;
				}
				case "DATA":
					if (this.remote?.writer) {
						await writeToWritable(this.remote.writer, payload);
						this._resetReadTimeout();
					}
					break;
				case "ERROR":
					this.log.warn("Client reported error:", payload);
					break;
				case "RAW":
					this.log.debug("Ignored raw message:", payload);
					break;
				default:
					this.log.debug("Unhandled message type:", type);
			}
		} catch (e) {
			this.log.error("⚡️ WS message handling error:", e);
			sendError(this.ws, e);
			await this._dispose();
		}
	}

	_onWsClose() { this._dispose(); }
	_onWsError(e) { this.log.error("🔴 WebSocket error:", e); this._dispose(); }
}

function parseAddress(addr) {
	if (!addr || typeof addr !== "string") throw new Error("Invalid address");
	addr = addr.trim();
	if (addr.startsWith("[") && addr.includes("]")) {
		const closeIdx = addr.indexOf("]");
		const host = addr.slice(1, closeIdx);
		const rest = addr.slice(closeIdx + 1);
		const colonIdx = rest.indexOf(":");
		const port = colonIdx >= 0 ? rest.slice(colonIdx + 1) : "";
		return { host, port };
	}
	const colonIndex = addr.lastIndexOf(":");
	if (colonIndex <= 0) throw new Error("Address must include port");
	const host = addr.slice(0, colonIndex);
	const port = addr.slice(colonIndex + 1);
	return { host, port };
}

function isIpAddress(host) {
	if (!host) return false;
	if (/^[0-9.]+$/.test(host)) return true;
	if (/^\[?[0-9a-fA-F:.]+\]?$/.test(host)) return true;
	return false;
}

const CF_ERROR_PATTERNS = [/proxy request/i, /cannot connect/i, /cloudflare/i];
function isCFError(err) {
	return CF_ERROR_PATTERNS.some(pattern => pattern.test(err?.message ?? ""));
}

function handleHttp(request) {
	const url = new URL(request.url);
	const path = url.pathname;
	if (path === "/ping") {
		return new Response(JSON.stringify({ status: "ok", ts: Date.now() }), {
			status: 200,
			headers: { "Content-Type": "application/json" },
		});
	}
	if (path === "/" || path === "/index.html") {
		return new Response("Hello World!", {
			status: 200,
			headers: { "Content-Type": "text/plain;charset=utf-8" },
		});
	}
	return new Response("Not Found", { status: 404 });
}

let globalPool = new SessionPool(DEFAULTS.MAX_SESSIONS);

export default {
	async fetch(request, env, ctx) {
		const cfg = new Config(request, env);
		globalLogger.level = cfg.LOG_LEVEL;
		globalPool.max = cfg.MAX_SESSIONS;
		if (request.headers.get("Upgrade")?.toLowerCase() !== "websocket") {
			return handleHttp(request);
		}
		const clientProtoHeader = request.headers.get("Sec-WebSocket-Protocol") ?? "";
		const clientProtocols = clientProtoHeader.split(",").map(s => s.trim()).filter(Boolean);
		if (cfg.TOKEN && !clientProtocols.includes(cfg.TOKEN)) {
			return new Response("Unauthorized", { status: 401 });
		}
		if (!globalPool.acquire()) {
			return new Response("Too many concurrent sessions", { status: 503 });
		}
		const [clientWs, serverWs] = Object.values(new WebSocketPair());
		try {
			serverWs.accept();
		} catch (e) {
			globalPool.release();
			return new Response("WebSocket accept failed", { status: 500 });
		}
		const session = new TcpWsSession(serverWs, cfg, globalLogger, () => globalPool.release());
		session.start();
		const respHeaders = { "Access-Control-Allow-Origin": cfg.ALLOW_ORIGIN };
		if (cfg.TOKEN && clientProtocols.includes(cfg.TOKEN)) {
			respHeaders["Sec-WebSocket-Protocol"] = cfg.TOKEN;
		}
		return new Response(null, { status: 101, webSocket: clientWs, headers: respHeaders });
	},
};
