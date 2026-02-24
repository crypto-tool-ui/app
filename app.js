#!/usr/bin/env node
/**
 * WebSocket to TCP Stratum Proxy - Production Ready
 * Dynamic target pool via base64 URL: ws://IP:PORT/base64(host:port)
 */

'use strict';

const WebSocket = require('ws');
const net = require('net');
const http = require('http');
const dns = require('dns').promises;
const crypto = require('crypto');

// ─── Configuration ────────────────────────────────────────────────────────────
const ALLOWED_HOSTS = "qrl.herominers.com,sal.herominers.com,sal.kryptex.network,pool.supportxmr.com,103.188.166.24";
const ALLOWED_PORTS = "3333,5555,7777,9000,443,80,1166,7028,1167,1230,1231,4567";
const CONFIG = {
  WS_PORT: 8000,

  // Security
  AUTH_TOKEN: process.env.AUTH_TOKEN || '',               // Bearer token (empty = disabled)
  ALLOWED_HOSTS: ALLOWED_HOSTS.split(',').map(h => h.trim().toLowerCase()),
  ALLOWED_PORTS: ALLOWED_PORTS.split(',').map(Number),

  // Limits
  MAX_CONNECTIONS: 25000,
  MAX_PAYLOAD_KB: 256, // Max WS message size (in KB)
  TCP_CONNECT_TIMEOUT_MS: parseInt(process.env.TCP_CONNECT_TIMEOUT_MS || '300000', 10), // 5 min
  TCP_IDLE_TIMEOUT_MS: parseInt(process.env.TCP_IDLE_TIMEOUT_MS || '300000', 10), // 5 min
  DNS_CACHE_TTL_MS: parseInt(process.env.DNS_CACHE_TTL_MS || '60000', 10),        // 1 min
};

// Warn if auth is disabled
if (!CONFIG.AUTH_TOKEN) {
  log('WARN', 'system', 'AUTH_TOKEN not set — proxy is open to anyone!');
}
if (CONFIG.ALLOWED_HOSTS.length === 0) {
  log('WARN', 'system', 'ALLOWED_HOSTS not set — any host is allowed!');
}

// ─── Structured Logger ────────────────────────────────────────────────────────
function log(level, reqId, message, extra = {}) {
  const entry = {
    ts: new Date().toISOString(),
    level,
    reqId,
    message,
    ...extra,
  };
  console.log(JSON.stringify(entry));
}

// ─── DNS Cache ────────────────────────────────────────────────────────────────
const dnsCache = new Map(); // host → { ip, expiresAt }

async function resolveHost(host, reqId) {
  // Return IP directly if already an IP
  if (net.isIP(host)) return host;

  const cached = dnsCache.get(host);
  if (cached && cached.expiresAt > Date.now()) {
    log('DEBUG', reqId, 'DNS cache hit', { host, ip: cached.ip });
    return cached.ip;
  }

  const addresses = await dns.resolve4(host);
  if (!addresses || addresses.length === 0) throw new Error(`No A record for ${host}`);
  const ip = addresses[0];
  dnsCache.set(host, { ip, expiresAt: Date.now() + CONFIG.DNS_CACHE_TTL_MS });
  log('DEBUG', reqId, 'DNS resolved', { host, ip });
  return ip;
}

// ─── Metrics ──────────────────────────────────────────────────────────────────
const metrics = {
  activeConnections: 0,
  totalConnections: 0,
  totalErrors: 0,
  bytesWsToTcp: 0,
  bytesTcpToWs: 0,
  startTime: Date.now(),
};

// ─── HTTP Server ──────────────────────────────────────────────────────────────
const server = http.createServer((req, res) => {
  if (req.url === '/health') {
    const healthy = metrics.activeConnections < CONFIG.MAX_CONNECTIONS;
    res.writeHead(healthy ? 200 : 503, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({
      status: healthy ? 'ok' : 'overloaded',
      activeConnections: metrics.activeConnections,
      maxConnections: CONFIG.MAX_CONNECTIONS,
      uptime: Math.floor((Date.now() - metrics.startTime) / 1000),
    }));
    return;
  }

  if (req.url === '/metrics') {
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({
      ...metrics,
      uptime: Math.floor((Date.now() - metrics.startTime) / 1000),
      dnsCacheSize: dnsCache.size,
    }));
    return;
  }

  res.writeHead(200, { 'Content-Type': 'text/plain' });
  res.end('WebSocket-TCP Stratum Proxy\n');
});

// ─── WebSocket Server ─────────────────────────────────────────────────────────
const wss = new WebSocket.Server({
  server,
  perMessageDeflate: false,
  maxPayload: CONFIG.MAX_PAYLOAD_KB * 1024,
});

wss.on('connection', async (ws, req) => {
  const reqId = crypto.randomBytes(4).toString('hex');
  const clientIp = req.headers['x-forwarded-for']?.split(',')[0]?.trim()
    || req.socket.remoteAddress;

  // ── 1. Connection limit ──────────────────────────────────────────────────
  if (metrics.activeConnections >= CONFIG.MAX_CONNECTIONS) {
    log('WARN', reqId, 'Connection limit reached, rejecting', { clientIp });
    ws.close(1013, 'Max connections reached');
    return;
  }

  // ── 2. Authentication ────────────────────────────────────────────────────
  if (CONFIG.AUTH_TOKEN) {
    const authHeader = req.headers['authorization'] || '';
    const token = authHeader.startsWith('Bearer ') ? authHeader.slice(7) : '';
    // Constant-time compare to prevent timing attacks
    const expected = Buffer.from(CONFIG.AUTH_TOKEN);
    const provided = Buffer.from(token.padEnd(CONFIG.AUTH_TOKEN.length));
    const valid = token.length === CONFIG.AUTH_TOKEN.length
      && crypto.timingSafeEqual(expected, provided);
    if (!valid) {
      log('WARN', reqId, 'Unauthorized connection', { clientIp });
      ws.close(1008, 'Unauthorized');
      return;
    }
  }

  // ── 3. Parse & validate target from URL path ─────────────────────────────
  const rawPath = req.url?.slice(1);
  if (!rawPath) {
    log('WARN', reqId, 'No path provided', { clientIp });
    ws.close(1008, 'Missing target');
    return;
  }

  let host, port;
  try {
    const decoded = Buffer.from(rawPath, 'base64').toString('utf8');
    const parts = decoded.split(':');
    if (parts.length !== 2) throw new Error('Expected host:port');
    host = parts[0].toLowerCase().trim();
    port = parseInt(parts[1], 10);
    if (!host || isNaN(port) || port < 1 || port > 65535) throw new Error('Invalid host or port');
  } catch (err) {
    log('WARN', reqId, 'Invalid target', { clientIp, error: err.message });
    ws.close(1008, 'Invalid target');
    return;
  }

  // ── 4. Whitelist checks ──────────────────────────────────────────────────
  if (CONFIG.ALLOWED_HOSTS.length > 0 && !CONFIG.ALLOWED_HOSTS.includes(host)) {
    log('WARN', reqId, 'Host not in whitelist', { clientIp, host });
    ws.close(1008, 'Host not allowed');
    return;
  }

  if (!CONFIG.ALLOWED_PORTS.includes(port)) {
    log('WARN', reqId, 'Port not in whitelist', { clientIp, host, port });
    ws.close(1008, 'Port not allowed');
    return;
  }

  // ── 5. DNS Resolution ────────────────────────────────────────────────────
  let resolvedIp;
  try {
    resolvedIp = await resolveHost(host, reqId);
  } catch (err) {
    log('ERROR', reqId, 'DNS resolution failed', { host, error: err.message });
    ws.close(1011, 'DNS resolution failed');
    return;
  }

  metrics.activeConnections++;
  metrics.totalConnections++;
  log('INFO', reqId, 'Connection established', { clientIp, host, resolvedIp, port });

  // ── 6. TCP Connection ────────────────────────────────────────────────────
  const tcpClient = new net.Socket();
  let tcpConnected = false;
  let closed = false;

  function closeAll(reason) {
    if (closed) return;
    closed = true;
    metrics.activeConnections--;
    log('INFO', reqId, 'Connection closed', { reason, host, port });
    tcpClient.destroy();
    if (ws.readyState === WebSocket.OPEN) ws.close();
  }

  // Connect timeout
  const connectTimer = setTimeout(() => {
    if (!tcpConnected) {
      log('ERROR', reqId, 'TCP connect timeout', { host, resolvedIp, port });
      closeAll('tcp_connect_timeout');
    }
  }, CONFIG.TCP_CONNECT_TIMEOUT_MS);

  tcpClient.connect(port, resolvedIp, () => {
    clearTimeout(connectTimer);
    tcpConnected = true;
    log('INFO', reqId, 'TCP connected', { host, resolvedIp, port });
  });

  tcpClient.setNoDelay(true);
  tcpClient.setKeepAlive(true, 30000);
  tcpClient.setTimeout(CONFIG.TCP_IDLE_TIMEOUT_MS);

  // ── 7. WS → TCP (with backpressure) ─────────────────────────────────────
  ws.on('message', (data) => {
    if (!tcpConnected || closed) return;
    try {
      const msg = data.toString('utf-8');
      const message = msg.endsWith('\n') ? msg : msg + '\n';
      metrics.bytesWsToTcp += message.length;
      const ok = tcpClient.write(message);
      if (!ok) {
        // TCP buffer full — pause WS until drained
        ws.pause();
        tcpClient.once('drain', () => ws.resume());
      }
    } catch (err) {
      log('ERROR', reqId, 'WS→TCP write failed', { error: err.message });
      metrics.totalErrors++;
    }
  });

  // ── 8. TCP → WS (with backpressure) ─────────────────────────────────────
  tcpClient.on('data', (data) => {
    if (ws.readyState !== WebSocket.OPEN) return;
    try {
      metrics.bytesTcpToWs += data.length;
      const text = data.toString('utf-8');

      ws.send(text, { binary: false }, (err) => {
        if (err) {
          log('ERROR', reqId, 'TCP→WS send failed', { error: err.message });
          metrics.totalErrors++;
        }
      });

      // Backpressure: pause TCP if WS send buffer is full
      if (ws.bufferedAmount > CONFIG.MAX_PAYLOAD_KB * 1024 * 4) {
        tcpClient.pause();
        const resume = setInterval(() => {
          if (ws.bufferedAmount === 0) {
            clearInterval(resume);
            tcpClient.resume();
          }
        }, 100);
      }
    } catch (err) {
      log('ERROR', reqId, 'TCP→WS failed', { error: err.message });
      metrics.totalErrors++;
    }
  });

  // ── 9. Cleanup handlers ───────────────────────────────────────────────────
  ws.on('close', (code, reason) => {
    closeAll(`ws_close:${code}`);
  });

  ws.on('error', (err) => {
    log('ERROR', reqId, 'WS error', { error: err.message });
    metrics.totalErrors++;
    closeAll('ws_error');
  });

  tcpClient.on('close', () => closeAll('tcp_close'));

  tcpClient.on('error', (err) => {
    log('ERROR', reqId, 'TCP error', { host, resolvedIp, port, error: err.message });
    metrics.totalErrors++;
    closeAll('tcp_error');
  });

  tcpClient.on('timeout', () => {
    log('WARN', reqId, 'TCP idle timeout', { host, port });
    closeAll('tcp_idle_timeout');
  });
});

wss.on('error', (err) => {
  log('ERROR', 'system', 'WSS error', { error: err.message });
});

// ─── Graceful Shutdown ────────────────────────────────────────────────────────
function shutdown(signal) {
  log('INFO', 'system', `Received ${signal}, shutting down gracefully...`);

  // Stop accepting new connections
  wss.close(() => {
    server.close(() => {
      log('INFO', 'system', 'Server closed. Goodbye.');
      process.exit(0);
    });
  });

  // Force exit after 15s if connections don't drain
  setTimeout(() => {
    log('WARN', 'system', 'Force exit after timeout');
    process.exit(1);
  }, 15000);
}

process.on('SIGTERM', () => shutdown('SIGTERM'));
process.on('SIGINT',  () => shutdown('SIGINT'));

process.on('uncaughtException', (err) => {
  log('ERROR', 'system', 'Uncaught exception', { error: err.message, stack: err.stack });
  metrics.totalErrors++;
});

process.on('unhandledRejection', (reason) => {
  log('ERROR', 'system', 'Unhandled rejection', { reason: String(reason) });
  metrics.totalErrors++;
});

// ─── Start ────────────────────────────────────────────────────────────────────
server.listen(CONFIG.WS_PORT, () => {
  log('INFO', 'system', 'Proxy started', {
    port: CONFIG.WS_PORT,
    maxConnections: CONFIG.MAX_CONNECTIONS,
    authEnabled: !!CONFIG.AUTH_TOKEN,
    allowedHosts: CONFIG.ALLOWED_HOSTS.length ? CONFIG.ALLOWED_HOSTS : 'ALL',
    allowedPorts: CONFIG.ALLOWED_PORTS,
  });
});
