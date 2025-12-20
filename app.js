import uWS from "uWebSockets.js";
import net from "net";
import cluster from "cluster";

const PORT = 8000;
const NUM_CPU = 2

if (cluster.isPrimary) {
	let online = 0;
	
    for (let index = 0; index < NUM_CPU; index++) {
        cluster.fork({
            isWorker: true
        })
    }

	cluster.on('message', (w, message) => {
		const { status, msg } = message;
		if (status) {
			online++;
		} else {
			online--;
		}
        console.log(msg + ` <-> WORKERS [${online}]`);
	})
} else {
    const app = uWS.App();

    // HTTP healthcheck
    app.get("/", (res) => res.end("ok"));

    // WebSocket <-> TCP proxy
    app.ws("/*", {
        compression: 0,
        maxPayloadLength: 16 * 1024 * 1024,
        idleTimeout: 300,
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

            // custom state
            const tcp = net.createConnection({
                host,
                port
            });
            ws.isConnected = false;
            ws.queue = [];
            ws.tcp = tcp;

            tcp.on("connect", () => {
                ws.isConnected = true;
                ws.queue.forEach(msg => tcp.write(msg.toString()));
                ws.queue.length = 0;
				        process.send({ status: true, msg: `🟢 SUCCESS: WS [${clientIp}] <-> TCP [${host}:${port}]` });
            });

            tcp.on("data", (data) => {
                try {
                    ws.send(data.toString());
                } catch (err) {
                    console.error("WS send failed:", err.message);
                }
            });

            tcp.on("close", () => {
                if (ws.isOpen) ws.end(1000, "TCP closed");
            });

            tcp.on("error", (err) => {
                console.error(`TCP error: ${err.message}`);
                if (ws.isOpen) ws.end(1011, err.message);
            });
        },

        message: (ws, msg) => {
            const data = Buffer.from(msg);
            if (ws.isConnected) {
                ws.tcp.write(data.toString());
            } else {
                ws.queue.push(data.toString());
            }
        },

        close: (ws) => {
            const clientIp = ws.ip;
            if (ws.tcp && !ws.tcp.destroyed) ws.tcp.destroy();
			      process.send({ status: false, msg: `🔴 DISCONNECTED: WS [${clientIp}]` });
        },
    });

    app.listen("0.0.0.0", PORT, (t) => {
        if (t) console.log(`🚀 WS⇄TCP proxy running on port ${PORT}`);
        else console.error("❌ Failed to listen");
    });
}
