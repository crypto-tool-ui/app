#!/usr/bin/env node
/**
 * WebSocket to TCP Stratum Proxy with DNS Resolution
 * Dynamic target pool via base64 URL:
 * ws://IP:PORT/base64(host:port)
 */
const WebSocket = require('ws');
const net = require('net');
const http = require('http');
const dns = require('dns').promises;

// Configuration
const WS_PORT = process.env.PORT || 8080;

// Create HTTP server
const server = http.createServer((req, res) => {
    res.writeHead(200, { 'Content-Type': 'text/plain' });
    res.end('WELCOME TO MCP-CLIENT-NODE PUBLIC! FEEL FREE TO USE! \n');
});

// WebSocket server
const wss = new WebSocket.Server({ 
    server,
    perMessageDeflate: false, // Disable compression for performance
    maxPayload: 100 * 1024, // 100KB max message size
});

console.log(`[PROXY] WebSocket listening on port: ${WS_PORT}`);
console.log(`[PROXY] Expected format: ws://IP:PORT/base64(host:port)`);
console.log(`[PROXY] Ready to accept connections...\n`);

wss.on('connection', async (ws, req) => {
    const clientIp = req.socket.remoteAddress;
    
    // --- Extract and decode target from URL ---
    const path = req.url?.slice(1); // remove leading "/"
    if (!path) {
        console.error(`[ERROR] No path provided from ${clientIp}`);
        ws.close();
        return;
    }
    
    let decoded, host, port;
    try {
        decoded = Buffer.from(path, 'base64').toString('utf8');
        [host, port] = decoded.split(':');
        if (!host || !port) throw new Error("Invalid target format");
    } catch (err) {
        console.error(`[ERROR] Base64 decode failed:`, err.message);
        ws.close();
        return;
    }
    
    // console.log(`[DNS] Resolving ${host} for client ${clientIp}...`);
    
    // --- DNS Lookup to get IP address | SUTO-00 ---
    let resolvedIp = host;
    // try {
    //     const addresses = await dns.resolve4(host);
    //     resolvedIp = addresses[0];
    // } catch (err) {
    // }
    
    console.log(`[WS] Connecting from ${clientIp} -> ${host} (${resolvedIp}):${port}`);
    
    // --- TCP connect to resolved IP ---
    const tcpClient = new net.Socket();
    tcpClient.connect(port, resolvedIp, () => {
        console.log(`[TCP] Connected from ${clientIp} -> ${host} (${resolvedIp}):${port}`);
    });
    tcpClient.setNoDelay(true);
    tcpClient.setKeepAlive(true, 30000);
    
    // --- WS → TCP ---
    ws.on('message', (data) => {
        try {
            const msg = data.toString('utf-8');
            const message = msg.endsWith("\n") ? msg : msg + "\n";
            tcpClient.write(message);
        } catch (err) {
            console.error(`[ERROR] WS→TCP failed:`, err.message);
        }
    });
    
    // --- TCP → WS ---
    tcpClient.on('data', (data) => {
        if (ws.readyState === WebSocket.OPEN) {
            try {
                const text = data.toString('utf-8');
                ws.send(text, { binary: false });
            } catch (err) {
                console.error(`[ERROR] TCP→WS:`, err.message);
            }
        }
    });
    
    // --- Cleanup ---
    ws.on('close', () => {
        // console.log(`[WS] Connection closed from ${clientIp}`);
        tcpClient.end();
    });
    
    ws.on('error', (err) => {
        // console.error(`[WS ERROR]`, err.message);
        tcpClient.end();
    });
    
    tcpClient.on('close', () => {
        console.log(`[TCP] Pool socket closed for ${host} (${resolvedIp}):${port}`);
        if (ws.readyState === WebSocket.OPEN) ws.close();
    });
    
    tcpClient.on('error', (err) => {
        console.error(`[TCP ERROR] ${host} (${resolvedIp}):${port}:`, err.message);
        if (ws.readyState === WebSocket.OPEN) ws.close();
    });
    
    tcpClient.on('timeout', () => {
        // console.log(`[TCP] Timeout for ${host} (${resolvedIp}):${port}`);
        tcpClient.end();
    });
});

wss.on('error', (err) => console.error(`[WSS ERROR]`, err.message));

// Start server
server.listen(WS_PORT, () => console.log(`[SERVER] Listening on port ${WS_PORT}`));
