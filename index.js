import uWS from "uWebSockets.js";
import net from "net";

const PORT = 8000;
const app = uWS.App();

// Cấu hình giới hạn
const MAX_ENCODED_LENGTH = 1024;       // tối đa 1KB cho string base64 host:port
const MAX_QUEUE_SIZE = 100;           // tối đa 1000 message trong queue
const MAX_PAYLOAD_LENGTH = 1024 * 1024; // 1MB WebSocket payload (tùy chỉnh lại nếu cần)
const IDLE_TIMEOUT_SECONDS = 300;      // 5 phút

const DEBUG = true;

// Chuẩn hóa message: đảm bảo có newline cuối
function normalizeLine(msg) {
    const text = typeof msg === "string" ? msg : String(msg);
    return text.endsWith("\n") ? text : text + "\n";
}

function safeEndWS(ws, code, reason) {
    try {
        if (ws.isClosed) return;
        if (ws.isOpen) {
            ws.end(code, reason);
        }
    } catch (e) {
        console.error("Error closing WebSocket:", e);
    }
}

/**
 * Giới hạn host nếu muốn tránh nội bộ, ví dụ:
 * - "127.0.0.1", "localhost"
 * - private ranges, v.v.
 * Tùy nhu cầu, hiện tại chỉ minh hoạ.
 */
function isForbiddenHost(host) {
    // Ví dụ đơn giản, có thể bỏ nếu bạn muốn full proxy
    const lower = host.toLowerCase();
    const allows = process.env.ALLOW_HOSTS || "";
    
    if (allows) {
        const list = allows.split(",").map(h => h.trim().toLowerCase());
        if (list.includes(lower)) return false;
    }

    return false;
}

// HTTP healthcheck
app.get("/", (res) => {
    res.writeHeader("Content-Type", "text/plain; charset=utf-8");
    res.writeHeader("Cache-Control", "no-store");
    res.end("MCP SERVER STATUS: RUNNING!");
});

// WebSocket <-> TCP proxy
app.ws("/*", {
    compression: 0,
    maxPayloadLength: MAX_PAYLOAD_LENGTH,
    idleTimeout: IDLE_TIMEOUT_SECONDS,

    upgrade: (res, req, context) => {
        try {
            const encoded = req.getUrl().slice(1);
            if (!encoded || encoded.length > MAX_ENCODED_LENGTH) {
                res.writeStatus("400 Bad Request").end("Invalid encoded length");
                return;
            }
            const ip = Buffer.from(res.getRemoteAddressAsText()).toString();

            res.upgrade(
                {
                    encoded,
                    ip,
                    isConnected: false,
                    queue: [],
                    tcp: null,
                    tcpHost: null,
                },
                req.getHeader("sec-websocket-key"),
                req.getHeader("sec-websocket-protocol"),
                req.getHeader("sec-websocket-extensions"),
                context
            );
        } catch (e) {
            console.error("Upgrade error:", e);
            try {
                res.writeStatus("500 Internal Server Error").end("Upgrade failed");
            } catch {
                // ignore
            }
        }
    },

    open: (ws) => {
        let decoded;
        try {
            if (typeof ws.encoded !== "string" || ws.encoded.length === 0) {
                safeEndWS(ws, 1011, "Missing encoded address");
                return;
            }

            decoded = Buffer.from(ws.encoded, "base64").toString("utf8");
        } catch {
            safeEndWS(ws, 1011, "Invalid base64");
            return;
        }

        const [host, portStr] = decoded.split(":");
        const port = Number.parseInt(portStr || "", 10);

        if (!host || !Number.isInteger(port) || port < 1 || port > 65535) {
            safeEndWS(ws, 1011, "Invalid address");
            return;
        }

        if (isForbiddenHost(host)) {
            safeEndWS(ws, 1011, "Forbidden target");
            return;
        }

        const clientIp = ws.ip;
        const tcp = net.createConnection({ host, port });
        tcp.setTimeout(0);
        tcp.setNoDelay(true);

        ws.isConnected = false;
        ws.queue = [];
        ws.tcp = tcp;
        ws.tcpHost = `${host}:${port}`;

        if (DEBUG) {
            console.log(`Connecting WS [${clientIp}] -> TCP [${host}:${port}]`);
        }

        tcp.on("connect", () => {
            ws.isConnected = true;

            // flush queue
            for (const msg of ws.queue) {
                tcp.write(normalizeLine(msg));
            }
            ws.queue.length = 0;

            console.log(`🟢 SUCCESS: WS [${clientIp}] <-> TCP [${host}:${port}]`);
        });

        tcp.on("data", (data) => {
            try {
                if (ws.isClosed) return;
                ws.send(data.toString("utf-8"), false);
            } catch (err) {
                console.error("WS send failed:", err?.message ?? err);
            }
        });

        tcp.on("close", () => {
            if (DEBUG) {
                console.log(`TCP closed [${host}:${port}] for WS [${clientIp}]`);
            }
            safeEndWS(ws, 1000, "TCP closed");
        });

        tcp.on("error", (err) => {
            console.error(`TCP error [${host}:${port}] for WS [${clientIp}]:`, err.message);
            safeEndWS(ws, 1011, err.message || "TCP error");
        });
    },

    // Helper nếu bạn cần gọi từ nơi khác
    tcpSend: (ws, msg) => {
        if (!ws.isConnected || !ws.tcp || ws.tcp.destroyed) return;
        ws.tcp.write(normalizeLine(msg));
    },

    message: (ws, msg) => {
        const data = Buffer.from(msg);
        const text = data.toString("utf-8");

        if (ws.isConnected && ws.tcp && !ws.tcp.destroyed) {
            ws.tcp.write(normalizeLine(text));
        } else {
            if (!Array.isArray(ws.queue)) {
                ws.queue = [];
            }

            if (ws.queue.length >= MAX_QUEUE_SIZE) {
                console.warn(
                    `Queue overflow for WS [${ws.ip}] -> ${ws.tcpHost || "unknown target"}`
                );
                safeEndWS(ws, 1011, "Queue overflow");
                return;
            }

            ws.queue.push(text);
        }
    },

    close: (ws, code, message) => {
        const clientIp = ws.ip;
        const tcpHost = ws.tcpHost;

        if (ws.tcp && !ws.tcp.destroyed) {
            try {
                ws.tcp.destroy();
            } catch (e) {
                console.error("Error destroying TCP socket:", e);
            }
        }

        // Dọn state
        ws.isConnected = false;
        ws.queue = [];
        ws.tcp = null;

        const reason = Buffer.from(message || "").toString("utf-8") || "no reason";
        console.log(
            `🔴 DISCONNECTED: WS [${clientIp}] <-> TCP [${tcpHost}] (code=${code}, reason="${reason}")`
        );
    },
});

app.listen("0.0.0.0", PORT, (t) => {
    if (t) {
        console.log(`🚀 WS⇄TCP proxy running on port ${PORT}`);
    } else {
        console.error("❌ Failed to listen");
    }
});
