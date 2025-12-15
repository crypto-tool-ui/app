#!/usr/bin/env node
/**
 * WebSocket to TCP Stratum Proxy (uWebSockets.js)
 * Dynamic target pool via base64 URL:
 * ws://IP:PORT/base64(host:port)
 */

import uWS from 'uWebSockets.js';
import net from 'net';

const WS_PORT = Number(process.argv[2] || 8000);

console.log(`[PROXY] WebSocket listening on port: ${WS_PORT}`);
console.log(`[PROXY] Expected format: ws://IP:PORT/base64(host:port)`);
console.log(`[PROXY] Ready to accept connections...`);

uWS.App()

/* ---------- HTTP fallback ---------- */
.any('/*', (res, req) => {
  res.writeHeader('Content-Type', 'text/plain');
  res.end('WELCOME TO MCP-CLIENT-NODE PUBLIC! FEEL FREE TO USE!\n');
})

/* ---------- WebSocket ---------- */
.ws('/*', {
  compression: uWS.DISABLED,          // perMessageDeflate = false
  maxPayloadLength: 100 * 1024,       // 100KB
  idleTimeout: 300,                   // seconds
  sendPingsAutomatically: false,
  
  upgrade(res, req, context) {
    res.upgrade(
      {
        url: req.getUrl(),
        ip: Buffer.from(res.getRemoteAddressAsText()).toString()
      },
      req.getHeader('sec-websocket-key'),
      req.getHeader('sec-websocket-protocol'),
      req.getHeader('sec-websocket-extensions'),
      context
    );
  },
  
  open(ws) {
    const { url, ip } = ws.getUserData();
    const clientIp = ip;

    // ---- decode base64 target ----
    const path = url.slice(1);
    if (!path) {
      ws.send(JSON.stringify({ error: 'Missing base64 target in URL' }));
      ws.close();
      return;
    }

    let host, port;
    try {
      const decoded = Buffer.from(path, 'base64').toString('utf8');
      [host, port] = decoded.split(':');
      if (!host || !port) throw new Error('Invalid target');
    } catch (e) {
      ws.send(JSON.stringify({ error: 'Invalid base64 target' }));
      ws.close();
      return;
    }

    console.log(`[WS] Connecting from ${clientIp} -> ${host}:${port}`);

    // ---- TCP socket ----
    const tcpClient = new net.Socket();
    ws.tcpClient = tcpClient;

    tcpClient.connect(Number(port), host, () => {
      console.log(`[WS] Connected from ${clientIp} -> ${host}:${port}`);
    });

    // TCP → WS
    tcpClient.on('data', (data) => {
      if (!ws.closed) {
        ws.send(data, true, false); // binary passthrough
      }
    });

    tcpClient.on('close', () => {
      if (!ws.closed) ws.close();
    });

    tcpClient.on('error', () => {
      if (!ws.closed) ws.close();
    });

    tcpClient.setTimeout(300000, () => {
      tcpClient.end();
    });
  },

  message(ws, message, isBinary) {
    // WS → TCP
    try {
      ws.tcpClient?.write(Buffer.from(message));
      ws.tcpClient?.write('\n');
    } catch {}
  },

  close(ws) {
    ws.tcpClient?.end();
  }
})

.listen(WS_PORT, (token) => {
  if (token) {
    console.log(`[SERVER] Listening on port ${WS_PORT}`);
  } else {
    console.error('[SERVER] Failed to listen');
  }
});
