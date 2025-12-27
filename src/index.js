import { DurableObject } from "cloudflare:workers";
import { connect } from 'cloudflare:sockets';

const CONFIG = {
	TOKEN: '123456',									// 填入访问令牌，留空则不验证
	CF_FALLBACK_IPS: [atob("UHJveHlJUC5DTUxpdXNzc3MubmV0"), atob("YnBiLnlvdXNlZi5pc2VnYXJvLmNvbQ=="), "84.235.245.18:34501"],
	LOG_LEVEL: 'none',								// 日志模式('debug' | 'warn' | 'error' | 'none')，debug 模式下会记录很多日志，记得要改为none无日志模式
	WRITE_TIMEOUT_MS: 5000,						// 超时用于远端写入操作（ms）
	WRITE_RETRY_COUNT: 2,							// 写失败重试次数
	WRITE_RETRY_DELAY_MS: 100,				// 重试间隔（ms）
	FORWARDING_MODE: 'native_loop',		// 数据转发模式('native_loop' 极速原生循环(推荐) | 'pipe_to' 标准pipeTo | 'manual_loop' 基础手动循环 )
};

function makeLogger(config, tag) {
	return {
		debug: (...args) => config.LOG_LEVEL === 'debug' && console.log(`[${tag} Debug]`, ...args),
		warn: (...args) => ['debug', 'warn'].includes(config.LOG_LEVEL) && console.warn(`[${tag} Warn]`, ...args),
		error: (...args) => ['debug', 'warn', 'error'].includes(config.LOG_LEVEL) && console.error(`[${tag} Error]`, ...args),
	};
}
const log = makeLogger(CONFIG, 'Session');

/**
 * Durable Object for managing WebSocket sessions.
 * @typedef {Object} Env
 * @property {DurableObjectNamespace} ECH_SERVER - The Durable Object namespace binding
 */
export class EchServer extends DurableObject {
	constructor(state, env) {
		super(state, env);
		// 复用模块层级的 CONFIG，保持单一配置来源
		this.CONFIG = Object.assign({}, CONFIG);
		// 从 env 覆盖配置（按需解析常见类型）
		if (env && typeof env === 'object') {
			try {
				if (env.FORWARDING_MODE) this.CONFIG.FORWARDING_MODE = env.FORWARDING_MODE;
				if (env.TOKEN) this.CONFIG.TOKEN = env.TOKEN;
				if (env.LOG_LEVEL) this.CONFIG.LOG_LEVEL = env.LOG_LEVEL;
				if (env.WRITE_TIMEOUT_MS) this.CONFIG.WRITE_TIMEOUT_MS = Number(env.WRITE_TIMEOUT_MS) || this.CONFIG.WRITE_TIMEOUT_MS;
				if (env.WRITE_RETRY_COUNT) this.CONFIG.WRITE_RETRY_COUNT = Number(env.WRITE_RETRY_COUNT) || this.CONFIG.WRITE_RETRY_COUNT;
				if (env.WRITE_RETRY_DELAY_MS) this.CONFIG.WRITE_RETRY_DELAY_MS = Number(env.WRITE_RETRY_DELAY_MS) || this.CONFIG.WRITE_RETRY_DELAY_MS;
				if (env.CF_FALLBACK_IPS) {
					const s = String(env.CF_FALLBACK_IPS).trim();
					if (s.length) this.CONFIG.CF_FALLBACK_IPS = s.split(',').map(x => x.trim()).filter(Boolean);
				}
			} catch (e) { /* ignore malformed env values, keep defaults */ }
		}
		this.PROTOCOL = {
			CMD_CONNECT: 'CONNECT:',
			CMD_DATA: 'DATA:',
			CMD_CLOSE: 'CLOSE',
			STATUS_CONNECTED: 'CONNECTED',
			STATUS_ERROR: 'ERROR:',
		};
		this.TEXT_ENCODER = new TextEncoder();
		this.log = makeLogger(this.CONFIG, 'DO');
	}

	createSessionLogger(sid) {
		return {
			debug: (...args) => this.log.debug(`[sid:${sid}]`, ...args),
			warn: (...args) => this.log.warn(`[sid:${sid}]`, ...args),
			error: (...args) => this.log.error(`[sid:${sid}]`, ...args),
		};
	}

	parseAddress(addr) {
		if (addr[0] === '[') {
			const end = addr.indexOf(']');
			if (end === -1) throw new Error('Invalid IPv6 address');
			return { host: addr.substring(1, end), port: parseInt(addr.substring(end + 2), 10) || 443 };
		}
		const sep = addr.lastIndexOf(':');
		if (sep === -1) throw new Error('Invalid address format');
		return { host: addr.substring(0, sep), port: parseInt(addr.substring(sep + 1), 10) || 443 };
	}

	parseFallbackEntry(entry) {
		if (typeof entry !== 'string' || entry.length === 0) return null;
		if (entry.startsWith('/')) {
			const last = entry.split('/').filter(Boolean).pop();
			if (!last) return null;
			const m = last.match(/^(.+)-(\d{1,5})$/);
			if (!m) return null;
			const port = Number(m[2]);
			if (!Number.isInteger(port) || port <= 0 || port > 65535) return null;
			return { host: m[1], port };
		}
		if (entry.startsWith('[')) {
			const m = entry.match(/^\[([^\]]+)\](?::(\d{1,5}))?$/);
			if (!m) return null;
			const host = m[1];
			const port = m[2] ? Number(m[2]) : undefined;
			if (port !== undefined && (!Number.isInteger(port) || port <= 0 || port > 65535)) return null;
			return { host, port };
		}
		const hyphen = entry.match(/^(.+)-(\d{1,5})$/);
		if (hyphen) {
			const port = Number(hyphen[2]);
			if (!Number.isInteger(port) || port <= 0 || port > 65535) return null;
			return { host: hyphen[1], port };
		}
		const sep = entry.lastIndexOf(':');
		if (sep === -1) return { host: entry, port: undefined };
		const host = entry.slice(0, sep);
		const port = Number(entry.slice(sep + 1));
		if (!Number.isInteger(port) || port <= 0 || port > 65535) return null;
		return { host, port };
	}

	normalizeFallbacks(list) {
		return Array.isArray(list) ? list.map((e) => this.parseFallbackEntry(e)).filter(p => p && p.host) : [];
	}

	isCfError(err) {
		const msg = err?.message?.toLowerCase() || '';
		return msg.includes('proxy request') || msg.includes('cannot connect') || msg.includes('cloudflare');
	}

	safeCloseWebSocket(ws) {
		try {
			if (ws.readyState === WebSocket.READY_STATE_OPEN || ws.readyState === WebSocket.READY_STATE_CLOSING) {
				ws.close(1000, 'Server closed');
			}
		} catch (e) {
			this.log.error('Error closing WS:', e);
		}
	}

	webSocketSession(clientSocket, pathFallback = null) {
		const doInst = this;

		class wsSession {
			constructor(clientSocket, pathFallback) {
				this.do = doInst;
				this.clientSocket = clientSocket;
				this.remoteSocket = null;
				this.remoteWriter = null;
				this.remoteReader = null;
				this.remoteReadable = null;
				this.isClosed = false;
				this.sessionId = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`;
				this.bytesToRemote = 0;
				this.bytesFromRemote = 0;
				this.slog = this.do.createSessionLogger(this.sessionId);
				this.pathFallback = pathFallback;
				this.startTime = null;
				this.endTime = null;
				this.lastActive = null;
				this._writeQueue = Promise.resolve();
				this.writeAttempts = 0;
				this.writeTimeouts = 0;
				this.writeErrors = 0;
				this.writeRetries = 0;
			}

			start() {
				this.clientSocket.accept();
				this.startTime = Date.now();
				this.lastActive = this.startTime;
				this.slog.debug('Client WebSocket accepted and session started');
				this.clientSocket.addEventListener('message', (event) => this.handleMessage(event));
				this.clientSocket.addEventListener('close', () => this.close());
				this.clientSocket.addEventListener('error', (err) => { this.slog.error('Client WebSocket error:', err); this.close(); });
			}

			async handleMessage(event) {
				if (this.isClosed) return;
				try {
					const data = event.data;
					if (typeof data === 'string') {
						this.slog.debug('Received text message from client, length:', data.length);
					} else if (data instanceof ArrayBuffer) {
						this.slog.debug('Received binary message from client, bytes:', data.byteLength);
					} else {
						this.slog.debug('Received message from client, type:', typeof data);
					}
					if (typeof data === 'string') {
						if (data.startsWith(this.do.PROTOCOL.CMD_CONNECT)) {
							const cmdEnd = this.do.PROTOCOL.CMD_CONNECT.length;
							const sepIndex = data.indexOf('|', cmdEnd);
							const targetAddr = data.substring(cmdEnd, sepIndex);
							const firstFrame = data.substring(sepIndex + 1);
							await this.connectToRemote(targetAddr, firstFrame);
						} else if (data.startsWith(this.do.PROTOCOL.CMD_DATA)) {
							if (this.remoteWriter) {
								const payload = this.do.TEXT_ENCODER.encode(data.substring(5));
								this.slog.debug('Queueing write to remote, bytes:', payload.byteLength);
								await this._writeRemote(payload);
							} else {
								this.slog.warn('Received DATA but no remote writer');
							}
						} else if (data === this.do.PROTOCOL.CMD_CLOSE) {
							this.close();
						}
					} else if (data instanceof ArrayBuffer && this.remoteWriter) {
						const bin = new Uint8Array(data);
						this.slog.debug('Queueing binary write to remote, bytes:', bin.byteLength);
						await this._writeRemote(bin);
					}
				} catch (err) {
					this._handleError(err);
				}
			}

			async connectToRemote(targetAddr, firstFrameData) {
				const { host, port } = this.do.parseAddress(targetAddr);
				let parsedFallbacksList;

				if (this.pathFallback) parsedFallbacksList = [this.pathFallback];
				else parsedFallbacksList = this.do.normalizeFallbacks(this.do.CONFIG.CF_FALLBACK_IPS);

				const attempts = [null, ...parsedFallbacksList];
				for (let i = 0; i < attempts.length; i++) {
					const attempt = attempts[i];
					const attemptHost = attempt ? attempt.host : host;
					const attemptPort = attempt && attempt.port ? attempt.port : port;
					try {
						this.slog.debug(`Connecting to ${attemptHost}:${attemptPort} (Attempt ${i + 1})`);
						this.remoteSocket = connect({ hostname: attemptHost, port: attemptPort });
						if (this.remoteSocket.opened) await this.remoteSocket.opened;
						this.slog.debug(`remote socket opened`);

						try {
							this.remoteWriter = this.remoteSocket.writable.getWriter();
							this.slog.debug(`Acquired remote writer`);
							if (firstFrameData) {
								const ff = this.do.TEXT_ENCODER.encode(firstFrameData);
								this.slog.debug(`Queueing first frame write to remote, bytes:`, ff.byteLength);
								await this._writeRemote(ff);
							}
						} catch (werr) {
							this.slog.error(`Failed to acquire/write remote writer:`, werr);
							this.cleanupRemoteResources();
							throw werr;
						}

						try {
							this.clientSocket.send(this.do.PROTOCOL.STATUS_CONNECTED);
							this.slog.debug(`Notified client: connected`);
						} catch (sendErr) {
							this.do.log.warn(`[sid:${this.sessionId}] Failed to notify client connected:`, sendErr);
						}

						switch (this.do.CONFIG.FORWARDING_MODE) {
							case 'pipe_to':
								this.startPipeTo();
								break;
							case 'native_loop':
								this.startNativeLoop();
								break;
							case 'manual_loop':
							default:
								this.startManualLoop();
								break;
						}
						return;
					} catch (err) {
						this.slog.warn(`Connection attempt ${i + 1} to ${attemptHost}:${attemptPort} failed:`, err.message);
						if (this.do.CONFIG.LOG_LEVEL === 'debug') this.slog.debug(err.stack);
						this.cleanupRemoteResources();
						if (!this.do.isCfError(err) || i === attempts.length - 1) throw err;
					}
				}
			}

			startManualLoop() {
				this.slog.debug(`Starting manual loop forwarding`);
				try {
					this.remoteReader = this.remoteSocket.readable.getReader();
					this.slog.debug(`Acquired remote reader (manual)`);
				} catch (rerr) {
					this.slog.error(`getReader failed (manual):`, rerr);
					this.close();
					return;
				}
				this.runManualLoop(this.remoteReader).catch(() => this.close());
			}

			async runManualLoop(reader) { return this._runLoop(reader, 'manual'); }

			startPipeTo() {
				this.slog.debug(`Starting pipeTo forwarding`);
				this.remoteReadable = this.remoteSocket.readable;
				const wsSink = new WritableStream({
					write: (chunk) => {
						if (this.clientSocket.readyState !== WebSocket.READY_STATE_OPEN) throw new Error('WS closed');
						const bytes = chunk?.byteLength || chunk?.length || 0;
						this.bytesFromRemote += bytes;
						this.slog.debug('Forwarding chunk to client (pipeTo), bytes:', bytes);
						try { this.clientSocket.send(chunk); } catch (sendErr) { this.slog.warn('Failed sending chunk to client (pipeTo):', sendErr); throw sendErr; }
					},
					close: () => this.close(),
					abort: () => this.close()
				});

				this.remoteReadable.pipeTo(wsSink).catch((pErr) => { this.slog.error('pipeTo failed/aborted:', pErr); if (!this.isClosed) this.close(); });
			}

			startNativeLoop() {
				this.slog.debug(`Starting native loop forwarding`);
				try {
					this.remoteReader = this.remoteSocket.readable.getReader();
					this.slog.debug(`Acquired remote reader (native)`);
				} catch (rerr) {
					this.slog.error(`getReader failed (native):`, rerr);
					this.close();
					return;
				}
				this.runNativeLoop(this.remoteReader).catch(() => this.close());
			}

			async runNativeLoop(reader) { return this._runLoop(reader, 'native'); }

			async _runLoop(reader, modeLabel) {
				try {
					while (!this.isClosed) {
						const { done, value } = await reader.read();
						if (done) break;
						if (this.clientSocket.readyState !== WebSocket.READY_STATE_OPEN) break;
						const bytes = value?.byteLength || value?.length || 0;
						this.bytesFromRemote += bytes;
						this.slog.debug(`Forwarding chunk to client (${modeLabel}), bytes:`, bytes);
						try { this.clientSocket.send(value); } catch (sendErr) { this.slog.warn(`Failed sending chunk to client (${modeLabel}):`, sendErr); break; }
					}
				} catch (e) {
				} finally { if (!this.isClosed) this.close(); }
			}

			async _writeRemote(data) {
				if (!this.remoteWriter) throw new Error('No remote writer');
				this._writeQueue = this._writeQueue.then(async () => {
					this.writeAttempts += 1;
					this.lastActive = Date.now();
					const timeoutMs = this.do.CONFIG.WRITE_TIMEOUT_MS || 5000;
					const maxRetries = Number.isInteger(this.do.CONFIG.WRITE_RETRY_COUNT) ? this.do.CONFIG.WRITE_RETRY_COUNT : 0;
					const retryDelay = this.do.CONFIG.WRITE_RETRY_DELAY_MS || 100;
					let attempt = 0;
					while (true) {
						attempt += 1;
						let timeoutHandle;
						try {
							const writePromise = this.remoteWriter.write(data);
							const timeoutPromise = new Promise((_, rej) => { timeoutHandle = setTimeout(() => rej(new Error('write timeout')), timeoutMs); });
							await Promise.race([writePromise, timeoutPromise]);
							clearTimeout(timeoutHandle);
							const bytes = data?.byteLength || data?.length || 0;
							this.bytesToRemote += bytes;
							if (attempt > 1) this.writeRetries += (attempt - 1);
							this.slog.debug('Wrote to remote, bytes:', bytes, 'attempt:', attempt);
							break;
						} catch (e) {
							if (timeoutHandle) clearTimeout(timeoutHandle);
							this.writeErrors += 1;
							if (e && e.message === 'write timeout') this.writeTimeouts += 1;
							if (attempt - 1 >= maxRetries) {
								this.slog.error('Write failed after retries:', e);
								this._handleError(e);
								throw e;
							} else {
								this.slog.warn('Write attempt failed, retrying after delay:', attempt, e);
								await new Promise((res) => setTimeout(res, retryDelay));
								continue;
							}
						}
					}
				});
				return this._writeQueue;
			}

			_handleError(err, sendToClient = true) {
				try {
					this.slog.error('Error:', err);
					if (sendToClient) {
						try { this.clientSocket.send(this.do.PROTOCOL.STATUS_ERROR + (err?.message || String(err))); } catch (e) { this.slog.warn('Failed sending error to client:', e); }
					}
				} catch (e) {
					this.do.log.error('Unexpected error in _handleError:', e);
				} finally { try { this.close(); } catch (e) { } }
			}

			cleanupRemoteResources() {
				this.slog.debug('Cleaning up remote resources');
				if (this.remoteReadable) {
					try { this.remoteReadable.cancel(); this.slog.debug('remoteReadable.cancel()'); } catch (e) { this.do.log.warn(`[sid:${this.sessionId}] remoteReadable.cancel() failed:`, e); }
				}
				if (this.remoteReader) {
					try { this.remoteReader.cancel(); this.slog.debug('remoteReader.cancel()'); } catch (e) { this.slog.warn('remoteReader.cancel() failed:', e); }
					try { this.remoteReader.releaseLock(); this.slog.debug('remoteReader.releaseLock()'); } catch (e) { this.slog.warn('remoteReader.releaseLock() failed:', e); }
				}
				if (this.remoteWriter) {
					try { this.remoteWriter.releaseLock(); this.slog.debug('remoteWriter.releaseLock()'); } catch (e) { this.slog.warn('remoteWriter.releaseLock() failed:', e); }
				}
				if (this.remoteSocket) {
					try { this.remoteSocket.close(); this.slog.debug('remoteSocket.close()'); } catch (e) { this.slog.warn('remoteSocket.close() failed:', e); }
				}
				this.remoteReader = null;
				this.remoteReadable = null;
				this.remoteWriter = null;
				this.remoteSocket = null;
			}

			close() {
				if (this.isClosed) return;
				this.isClosed = true;
				this.endTime = Date.now();
				const durationMs = this.endTime - (this.startTime || this.endTime);
				const durationSec = Math.max(1, Math.floor(durationMs / 1000));
				const txRate = Math.round((this.bytesToRemote / durationSec) || 0);
				const rxRate = Math.round((this.bytesFromRemote / durationSec) || 0);
				this.slog.warn(`Session closing, cleaning resources; bytesToRemote=${this.bytesToRemote}, bytesFromRemote=${this.bytesFromRemote}, durationMs=${durationMs}, txBps=${txRate}, rxBps=${rxRate}, writeAttempts=${this.writeAttempts}, writeTimeouts=${this.writeTimeouts}, writeErrors=${this.writeErrors}`);
				this.cleanupRemoteResources();
				this.do.safeCloseWebSocket(this.clientSocket);
			}

			sendError(msg) {
				this.slog.error('Sending error to client:', msg);
				try { this.clientSocket.send(this.do.PROTOCOL.STATUS_ERROR + msg); } catch (e) { this.slog.warn('sendError failed:', e); }
			}
		}

		return new wsSession(clientSocket, pathFallback);
	}

	// Durable Object fetch: 仅处理 WebSocket 升级请求（Worker 端只会把 WS 请求转发到 DO）
	async fetch(request) {
		try {
			const upgradeHeader = request.headers.get('Upgrade');
			if (!upgradeHeader || upgradeHeader.toLowerCase() !== 'websocket') {
				return new Response('Expected WebSocket', { status: 426 });
			}

			// 认证（可选）
			if (this.CONFIG.TOKEN && request.headers.get('Sec-WebSocket-Protocol') !== this.CONFIG.TOKEN) {
				return new Response('Unauthorized', { status: 401 });
			}

			// 解析 URL Path 的 fallback 地址(需带端口host-port格式)
			let pathFallbackParsed = null;
			try {
				const url = new URL(request.url);
				const segs = url.pathname.split('/').filter(Boolean);
				if (segs.length > 0) {
					const lastSeg = segs[segs.length - 1];
					const parsed = this.parseFallbackEntry(lastSeg);
					if (parsed && parsed.host) pathFallbackParsed = parsed;
				}
			} catch (e) { this.log.warn('Failed parsing fallback from request path:', e); }

			const [client, server] = Object.values(new WebSocketPair());
			const session = this.webSocketSession(server, pathFallbackParsed);
			session.start();

			const responseInit = { status: 101, webSocket: client };
			if (this.CONFIG.TOKEN) responseInit.headers = { 'Sec-WebSocket-Protocol': this.CONFIG.TOKEN };
			return new Response(null, responseInit);
		} catch (err) {
			return new Response(err.toString(), { status: 500 });
		}
	}
}

export default {
	async fetch(request, env, ctx) {
		try {
			const pathname = new URL(request.url).pathname;
			const upgradeHeader = request.headers.get('Upgrade');
			if (!upgradeHeader || upgradeHeader.toLowerCase() !== 'websocket') {
				return new Response(pathname === '/' ? "hello world!" : "Not found!", { status: 200 });
			}
			try {
				const stub = env.ECH_SERVER.getByName('ECH');
				return await stub.fetch(request);
			} catch (e) {
				return new Response('DO error: ' + String(e), { status: 500 });
			}
		} catch (err) {
			return new Response(err.toString(), { status: 500 });
		}
	},
};
