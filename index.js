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
    compression: uWS.DISABLED,
    maxPayloadLength: 100 * 1024,
    idleTimeout: 300,
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

        // Thêm flag và queue để quản lý trạng thái
        ws.tcpReady = false;
        ws.messageQueue = [];
        
        // TCP socket
        const tcpClient = new net.Socket();
        ws.tcpClient = tcpClient;
        
        tcpClient.connect(Number(port), host, () => {
            connections++;
            console.log(`[WS] Connected from ${clientIp} -> ${host}:${port} [${connections} workers]`);
            
            // Đánh dấu TCP đã sẵn sàng
            ws.tcpReady = true;
            
            // Gửi tất cả message trong queue
            while (ws.messageQueue.length > 0) {
                const bufferedMsg = ws.messageQueue.shift();
                try {
                    tcpClient.write(bufferedMsg);
                } catch (err) {
                    console.error(`[WS] Error sending queued message:`, err);
                }
            }
        });
        
        // TCP → WS
        tcpClient.on('data', (data) => {
            if (ws.isOpen) {
                const msg = data.toString().trim();
                ws.send(msg);
            }
        });
        
        tcpClient.on('close', () => {
            ws.tcpReady = false;
            if (ws.isOpen) ws.end(1000, "TCP closed");
        });
        
        tcpClient.on('error', (err) => {
            ws.tcpReady = false;
            console.error(`[TCP] Error:`, err);
            if (ws.isOpen) ws.end(1011, "TCP error");
        });
        
        tcpClient.setTimeout(300000, () => {
            tcpClient.end();
        });
    },
    
    message(ws, message, isBinary) {
        // WS → TCP
        try {
            const buffer = Buffer.from(message);
            
            // Kiểm tra xem TCP đã sẵn sàng chưa
            if (ws.tcpReady && ws.tcpClient && !ws.tcpClient.destroyed) {
                // Gửi trực tiếp
                ws.tcpClient.write(buffer);
            } else {
                // Lưu vào queue nếu TCP chưa sẵn sàng
                ws.messageQueue.push(buffer);
                
                // Optional: Giới hạn kích thước queue để tránh memory leak
                if (ws.messageQueue.length > 100) {
                    console.warn(`[WS] Message queue overflow, dropping oldest message`);
                    ws.messageQueue.shift();
                }
            }
        } catch (err) {
            console.error(`[WS] Error handling message:`, err);
        }
    },
    
    close: (ws) => {
        const clientIp = ws.ip;
        connections--;
        console.log(`[WS] Disconnected from ${clientIp} [${connections} workers]`);
        
        // Cleanup
        ws.tcpReady = false;
        ws.messageQueue = [];
        
        if (ws.tcpClient && !ws.tcpClient.destroyed) {
            ws.tcpClient.destroy();
        }
    },
});

app.listen("0.0.0.0", PORT, (t) => {
    if (t) console.log(`🚀 WS⇄TCP proxy running on port ${PORT}`);
    else console.error("❌ Failed to listen");
});
