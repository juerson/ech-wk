import { connect } from 'cloudflare:sockets';

const CONFIG = {
	TOKEN: '', // 填入访问令牌，留空则不验证
	CF_FALLBACK_IPS: [atob("UHJveHlJUC5DTUxpdXNzc3MubmV0"), atob("YnBiLnlvdXNlZi5pc2VnYXJvLmNvbQ=="), "84.235.245.18:34501"], // 填入优选IP
	LOG_LEVEL: 'none', // 'debug' | 'warn' | 'error' | 'none'
	WRITE_TIMEOUT_MS: 5000, // 超时用于远端写入操作（ms）
	WRITE_RETRY_COUNT: 2, // 写失败重试次数
	WRITE_RETRY_DELAY_MS: 100, // 重试间隔（ms）
};

/**
 * FORWARDING_MODE：数据转发模式选择
 * @type {'native_loop' | 'manual_loop' | 'pipe_to'} - 极速原生循环(推荐) | 基础手动循环 | 标准流式API
 */
const FORWARDING_MODE = 'native_loop';
const TEXT_ENCODER = new TextEncoder();
const PROTOCOL = {
	CMD_CONNECT: 'CONNECT:',
	CMD_DATA: 'DATA:',
	CMD_CLOSE: 'CLOSE',
	STATUS_CONNECTED: 'CONNECTED',
	STATUS_ERROR: 'ERROR:',
};
const log = {
	debug: (...args) => CONFIG.LOG_LEVEL === 'debug' && console.log('[Session Debug]', ...args),
	warn: (...args) => ['debug', 'warn'].includes(CONFIG.LOG_LEVEL) && console.warn('[Session Warn]', ...args),
	error: (...args) => ['debug', 'warn', 'error'].includes(CONFIG.LOG_LEVEL) && console.error('[Session Error]', ...args),
};

// 会话级日志工厂，统一前缀和调用
function createSessionLogger(sid) {
	return {
		debug: (...args) => log.debug(`[sid:${sid}]`, ...args),
		warn: (...args) => log.warn(`[sid:${sid}]`, ...args),
		error: (...args) => log.error(`[sid:${sid}]`, ...args),
	};
}

function parseAddress(addr) {
	if (addr[0] === '[') {
		const end = addr.indexOf(']');
		if (end === -1) throw new Error('Invalid IPv6 address');
		return { host: addr.substring(1, end), port: parseInt(addr.substring(end + 2), 10) || 443 };
	}
	const sep = addr.lastIndexOf(':');
	if (sep === -1) throw new Error('Invalid address format');
	return { host: addr.substring(0, sep), port: parseInt(addr.substring(sep + 1), 10) || 443 };
}

/**
 * 解析 fallback 条目，支持三种形式：
 * - domain:port 或 ipv4:port 或 [ipv6]:port
 * - 路径形式，如 /to/path/host-port ，取最后一段并把最后一个 '-' 替换为 ':' -> host:port
 *
 * 返回 {host, port} 对象，无效时返回 null
 */
function parseFallbackEntry(entry) {
	if (typeof entry !== 'string' || entry.length === 0) return null;

	// 路径形式：仅当以 '/' 开头时，取最后一段并要求为 host-port
	if (entry.startsWith('/')) {
		const last = entry.split('/').filter(Boolean).pop();
		if (!last) return null;
		const m = last.match(/^(.+)-(\d{1,5})$/);
		if (!m) return null;
		const port = Number(m[2]);
		if (!Number.isInteger(port) || port <= 0 || port > 65535) return null;
		return { host: m[1], port };
	}

	// IPv6 带端口: [addr]:port
	if (entry.startsWith('[')) {
		const m = entry.match(/^\[([^\]]+)\](?::(\d{1,5}))?$/);
		if (!m) return null;
		const host = m[1];
		const port = m[2] ? Number(m[2]) : undefined;
		if (port !== undefined && (!Number.isInteger(port) || port <= 0 || port > 65535)) return null;
		return { host, port };
	}

	// 支持 host-port 短横分隔（如 example.com-8443 或 1.2.3.4-8443）
	const hyphen = entry.match(/^(.+)-(\d{1,5})$/);
	if (hyphen) {
		const port = Number(hyphen[2]);
		if (!Number.isInteger(port) || port <= 0 || port > 65535) return null;
		return { host: hyphen[1], port };
	}

	// 最后，host:port 或仅 host
	const sep = entry.lastIndexOf(':');
	if (sep === -1) return { host: entry, port: undefined };
	const host = entry.slice(0, sep);
	const port = Number(entry.slice(sep + 1));
	if (!Number.isInteger(port) || port <= 0 || port > 65535) return null;
	return { host, port };
}

// 归一化 CONFIG.CF_FALLBACK_IPS 为 {host,port} 数组，过滤无效项
function normalizeFallbacks(list) {
	return Array.isArray(list) ? list.map(parseFallbackEntry).filter(p => p && p.host) : [];
}

function isCfError(err) {
	const msg = err?.message?.toLowerCase() || '';
	return msg.includes('proxy request') || msg.includes('cannot connect') || msg.includes('cloudflare');
}

function safeCloseWebSocket(ws) {
	try {
		if (ws.readyState === WebSocket.READY_STATE_OPEN || ws.readyState === WebSocket.READY_STATE_CLOSING) {
			ws.close(1000, 'Server closed');
		}
	} catch (e) {
		// Ignore close errors
		log.error('Error closing WS:', e);
	}
}

class ProxySession {
	constructor(clientSocket, pathFallback = null) {
		this.clientSocket = clientSocket;
		this.remoteSocket = null;

		// Client -> Remote 资源
		this.remoteWriter = null;

		// Remote -> Client 资源
		this.remoteReader = null; // manual_loop / native_loop 模式使用 remoteReader
		this.remoteReadable = null; // pipe_to 模式使用 remoteReadable

		this.isClosed = false;
		this.sessionId = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`;
		this.bytesToRemote = 0;
		this.bytesFromRemote = 0;
		this.slog = createSessionLogger(this.sessionId);

		// 会话级的路径覆盖 fallback：{host,port} 或 null
		this.pathFallback = pathFallback;

		// 时序与统计
		this.startTime = null;
		this.endTime = null;
		this.lastActive = null;

		// 写入队列与背压统计
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
		this.clientSocket.addEventListener('error', (err) => {
			this.slog.error('Client WebSocket error:', err);
			this.close();
		});
	}

	async handleMessage(event) {
		if (this.isClosed) return;
		try {
			const data = event.data;
			// 简要日志：类型与长度
			if (typeof data === 'string') {
				this.slog.debug('Received text message from client, length:', data.length);
			} else if (data instanceof ArrayBuffer) {
				this.slog.debug('Received binary message from client, bytes:', data.byteLength);
			} else {
				this.slog.debug('Received message from client, type:', typeof data);
			}
			if (typeof data === 'string') {
				if (data.startsWith(PROTOCOL.CMD_CONNECT)) {
					const cmdEnd = PROTOCOL.CMD_CONNECT.length;
					const sepIndex = data.indexOf('|', cmdEnd);
					const targetAddr = data.substring(cmdEnd, sepIndex);
					const firstFrame = data.substring(sepIndex + 1);
					await this.connectToRemote(targetAddr, firstFrame);
				} else if (data.startsWith(PROTOCOL.CMD_DATA)) {
					if (this.remoteWriter) {
						const payload = TEXT_ENCODER.encode(data.substring(5));
						this.slog.debug('Queueing write to remote, bytes:', payload.byteLength);
						await this._writeRemote(payload);
					} else {
						this.slog.warn('Received DATA but no remote writer');
					}
				} else if (data === PROTOCOL.CMD_CLOSE) {
					this.close();
				}
			} else if (data instanceof ArrayBuffer && this.remoteWriter) {
				const bin = new Uint8Array(data);
				this.slog.debug('Queueing binary write to remote, bytes:', bin.byteLength);
				await this._writeRemote(bin);
			}
		} catch (err) {
			// 统一错误处理
			this._handleError(err);
		}
	}

	async connectToRemote(targetAddr, firstFrameData) {
		const { host, port } = parseAddress(targetAddr);
		let parsedFallbacksList;
		if (this.pathFallback) {
			parsedFallbacksList = [this.pathFallback];
		} else {
			parsedFallbacksList = normalizeFallbacks(CONFIG.CF_FALLBACK_IPS);
		}
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
						const ff = TEXT_ENCODER.encode(firstFrameData);
						this.slog.debug(`Queueing first frame write to remote, bytes:`, ff.byteLength);
						await this._writeRemote(ff);
					}
				} catch (werr) {
					this.slog.error(`Failed to acquire/write remote writer:`, werr);
					this.cleanupRemoteResources();
					throw werr;
				}

				try {
					this.clientSocket.send(PROTOCOL.STATUS_CONNECTED);
					this.slog.debug(`Notified client: connected`);
				} catch (sendErr) {
					log.warn(`[sid:${this.sessionId}] Failed to notify client connected:`, sendErr);
				}

				// 根据 FORWARDING_MODE 选择启动哪种转发逻辑
				log.debug('Forwarding mode selected:', FORWARDING_MODE);
				switch (FORWARDING_MODE) {
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
				if (CONFIG.LOG_LEVEL === 'debug') this.slog.debug(err.stack);
				this.cleanupRemoteResources();
				if (!isCfError(err) || i === attempts.length - 1) throw err;
			}
		}
	}

	// 模式 1: 基础手动循环
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

	async runManualLoop(reader) {
		return this._runLoop(reader, 'manual');
	}

	// 模式 2: 标准 pipeTo
	startPipeTo() {
		this.slog.debug(`Starting pipeTo forwarding`);
		this.remoteReadable = this.remoteSocket.readable;
		const wsSink = new WritableStream({
			write: (chunk) => {
				if (this.clientSocket.readyState !== WebSocket.READY_STATE_OPEN) {
					throw new Error('WS closed');
				}
				const bytes = chunk?.byteLength || chunk?.length || 0;
				this.bytesFromRemote += bytes;
				this.slog.debug('Forwarding chunk to client (pipeTo), bytes:', bytes);
				try {
					this.clientSocket.send(chunk);
				} catch (sendErr) {
					this.slog.warn('Failed sending chunk to client (pipeTo):', sendErr);
					throw sendErr;
				}
			},
			close: () => this.close(),
			abort: () => this.close()
		});

		this.remoteReadable.pipeTo(wsSink).catch((pErr) => {
			this.slog.error('pipeTo failed/aborted:', pErr);
			if (!this.isClosed) this.close();
		});
	}

	// 模式 3: 极速原生循环
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

	async runNativeLoop(reader) {
		return this._runLoop(reader, 'native');
	}

	async _runLoop(reader, modeLabel) {
		try {
			while (!this.isClosed) {
				const { done, value } = await reader.read();
				if (done) break;
				if (this.clientSocket.readyState !== WebSocket.READY_STATE_OPEN) break;
				const bytes = value?.byteLength || value?.length || 0;
				this.bytesFromRemote += bytes;
				this.slog.debug(`Forwarding chunk to client (${modeLabel}), bytes:`, bytes);
				try {
					this.clientSocket.send(value);
				} catch (sendErr) {
					this.slog.warn(`Failed sending chunk to client (${modeLabel}):`, sendErr);
					break;
				}
			}
		} catch (e) {
			// 读取中断通常意味着连接关闭或主动 cancel
		} finally {
			if (!this.isClosed) this.close();
		}
	}

	async _writeRemote(data) {
		if (!this.remoteWriter) throw new Error('No remote writer');
		// 排队写入并支持重试，防止并发写导致竞态或内存聚集
		this._writeQueue = this._writeQueue.then(async () => {
			this.writeAttempts += 1;
			this.lastActive = Date.now();
			const timeoutMs = CONFIG.WRITE_TIMEOUT_MS || 5000;
			const maxRetries = Number.isInteger(CONFIG.WRITE_RETRY_COUNT) ? CONFIG.WRITE_RETRY_COUNT : 0;
			const retryDelay = CONFIG.WRITE_RETRY_DELAY_MS || 100;
			let attempt = 0;
			while (true) {
				attempt += 1;
				let timeoutHandle;
				try {
					const writePromise = this.remoteWriter.write(data);
					const timeoutPromise = new Promise((_, rej) => {
						timeoutHandle = setTimeout(() => rej(new Error('write timeout')), timeoutMs);
					});
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
				try { this.clientSocket.send(PROTOCOL.STATUS_ERROR + (err?.message || String(err))); } catch (e) { this.slog.warn('Failed sending error to client:', e); }
			}
		} catch (e) {
			// logging the logging error - best-effort
			log.error('Unexpected error in _handleError:', e);
		} finally {
			try { this.close(); } catch (e) { /* ignore */ }
		}
	}

	// 资源清理
	cleanupRemoteResources() {
		this.slog.debug('Cleaning up remote resources');
		// 1. 清理 PipeTo 相关
		if (this.remoteReadable) {
			try {
				this.remoteReadable.cancel();
				this.slog.debug('remoteReadable.cancel()');
			} catch (e) { log.warn(`[sid:${this.sessionId}] remoteReadable.cancel() failed:`, e); }
		}

		// 2. 清理 Loop 相关 (关键：主动 cancel 以解除 read 阻塞)
		if (this.remoteReader) {
			try { this.remoteReader.cancel(); this.slog.debug('remoteReader.cancel()'); } catch (e) { this.slog.warn('remoteReader.cancel() failed:', e); }
			try { this.remoteReader.releaseLock(); this.slog.debug('remoteReader.releaseLock()'); } catch (e) { this.slog.warn('remoteReader.releaseLock() failed:', e); }
		}

		// 3. 清理 Writer
		if (this.remoteWriter) {
			try { this.remoteWriter.releaseLock(); this.slog.debug('remoteWriter.releaseLock()'); } catch (e) { this.slog.warn('remoteWriter.releaseLock() failed:', e); }
		}

		// 4. 关闭 Socket
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
		safeCloseWebSocket(this.clientSocket);
	}

	sendError(msg) {
		this.slog.error('Sending error to client:', msg);
		try { this.clientSocket.send(PROTOCOL.STATUS_ERROR + msg); } catch (e) { this.slog.warn('sendError failed:', e); }
	}
}

export default {
	async fetch(request, env, ctx) {
		try {
			const upgradeHeader = request.headers.get('Upgrade');
			if (!upgradeHeader || upgradeHeader.toLowerCase() !== 'websocket') {
				return new URL(request.url).pathname === '/'
					? new Response('Hello world!', { status: 200 })
					: new Response('Expected WebSocket', { status: 426 });
			}
			if (CONFIG.TOKEN && request.headers.get('Sec-WebSocket-Protocol') !== CONFIG.TOKEN) {
				return new Response('Unauthorized', { status: 401 });
			}

			// 检查请求路径最后一段是否包含覆盖用的 fallback 地址（如 /x/y/example.com-8443）
			let pathFallbackParsed = null;
			try {
				const url = new URL(request.url);
				const segs = url.pathname.split('/').filter(Boolean);
				if (segs.length > 0) {
					const lastSeg = segs[segs.length - 1];
					// 解析为 {host, port}，parseFallbackEntry 会把最后的 '-' 转为 ':' 并分离
					const parsed = parseFallbackEntry(lastSeg);
					if (parsed && parsed.host) {
						pathFallbackParsed = parsed;
					}
				}
			} catch (e) {
				log.warn('Failed parsing fallback from request path:', e);
			}

			const [client, server] = Object.values(new WebSocketPair());
			const session = new ProxySession(server, pathFallbackParsed);
			ctx.waitUntil(session.start());

			const responseInit = { status: 101, webSocket: client };
			if (CONFIG.TOKEN) {
				responseInit.headers = { 'Sec-WebSocket-Protocol': CONFIG.TOKEN };
			}
			return new Response(null, responseInit);

		} catch (err) {
			return new Response(err.toString(), { status: 500 });
		}
	},
};
