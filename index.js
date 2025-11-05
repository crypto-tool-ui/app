import net from "net";
import { App } from "uWebSockets.js";

const numCPUs = 2;
const WS_PORT = process.env.PORT || 8000;

 const app = App();

  // Healthcheck route
  app.get("/", (res) => res.writeStatus("200 OK").end("ok"));

  // WebSocket route: /<base64(host:port)>
  app.ws("/*", {
    compression: 0,
    maxPayloadLength: 16 * 1024 * 1024,
    idleTimeout: 60,

    open: (ws, req) => {
      try {
        const path = req.getUrl().slice(1); // remove "/"
        const decoded = Buffer.from(path, "base64").toString("utf8");
        const [host, port] = decoded.split(":");
        console.log(`[${process.pid}] WS connect → TCP ${host}:${port}`);

        const socket = net.createConnection({ host, port: parseInt(port, 10) });
        ws.tcp = socket;

        // TCP → WS
        socket.on("data", (chunk) => {
          if (ws) ws.send(chunk, true);
        });

        socket.on("close", () => {
          try { ws.end(1000, "TCP closed"); } catch {}
        });

        socket.on("error", (err) => {
          console.error("TCP error:", err.message);
          try { ws.end(1011, err.message); } catch {}
        });
      } catch (err) {
        console.error("Open error:", err);
        ws.end(1011, "Invalid address");
      }
    },

    message: (ws, message, isBinary) => {
      if (ws.tcp && !ws.tcp.destroyed) {
        ws.tcp.write(Buffer.from(message));
      }
    },

    close: (ws, code, message) => {
      if (ws.tcp && !ws.tcp.destroyed) ws.tcp.destroy();
      console.log(`[${process.pid}] WS closed (${code})`);
    },
  });

  // Start server
  app.listen(WS_PORT, (token) => {
    if (token) {
      console.log(`✅ Worker ${process.pid} listening on ${WS_PORT}`);
    } else {
      console.error(`❌ Worker ${process.pid} failed to listen`);
    }
  });
