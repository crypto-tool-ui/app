import uWS from "uWebSockets.js";
import net from "net";

const PORT = 8000;
const app = uWS.App();
let connections = 0;

// HTTP healthcheck
app.get("/", (res) => {
    res.writeHeader('Content-Type', 'text/plain');
    res.end('WELCOME TO MCP-CLIENT-NODE PUBLIC! FEEL FREE TO USE!\n');
});

// WebSocket <-> TCP proxy
app.ws("/*", {
    compression: uWS.DISABLED,          // perMessageDeflate = false
    maxPayloadLength: 100 * 1024,       // 100KB
    idleTimeout: 300,                   // seconds
    sendPingsAutomatically: false,

    upgrade: (res, req, context) => {
        res.upgrade(
            {
                encoded: req.getUrl().slice(1),
                ip: Buffer.from(res.getRemoteAddressAsText()).toString()
            },
            req.getHeader("sec-websocket-key"),
            req.getHeader("sec-websocket-protocol"),
            req.getHeader("sec-websocket-extensions"),
            context
        );
    },

    open: (ws) => {
        const decoded = Buffer.from(ws.encoded, "base64").toString("utf8");
        const clientIp = ws.ip;

        const [host, portStr] = decoded.split(":");
        const port = parseInt(portStr, 10);

        if (!host || !port) {
            ws.end(1011, "Invalid address");
            return;
        }

        // console.log(`[WS] Connecting from ${clientIp} -> ${host}:${port}`);

        // ---- TCP socket ----
        const tcpClient = new net.Socket();
        ws.tcpClient = tcpClient;

        tcpClient.connect(Number(port), host, () => {
            connections++;
            console.log(`[WS] Connected from ${clientIp} -> ${host}:${port} [${connections} workers]`);
        });

        // TCP → WS
        tcpClient.on('data', (data) => {
            if (ws.isOpen) {
                ws.send(data, true); // binary passthrough
            }
        });

        tcpClient.on('close', () => {
            if (ws.isOpen) ws.end(1000, "TCP closed");
        });

        tcpClient.on('error', () => {
            if (ws.isOpen) ws.end(1011, "TCP error");
        });

        tcpClient.setTimeout(300000, () => {
            tcpClient.end();
        });
    },

    message(ws, message, isBinary) {
        // WS → TCP
        try {
            ws.tcpClient?.write(Buffer.from(message));
        } catch { }
    },

    close: (ws) => {
        const clientIp = ws.ip;
        connections--;
        console.log(`[WS] Disconnected from ${clientIp} [${connections} workers]`);
        if (ws.tcpClient && !ws.tcpClient.destroyed) {
            ws.tcpClient.destroy();
        }
    },
});

app.listen("0.0.0.0", PORT, (t) => {
    if (t) console.log(`🚀 WS⇄TCP proxy running on port ${PORT}`);
    else console.error("❌ Failed to listen");
});
