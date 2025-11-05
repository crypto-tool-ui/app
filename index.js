import { App } from "uWebSockets.js";
import net from "net";

const PORT = 8000;

const app = App();

app.get("/", (res) => {
  res.writeStatus("200 OK").end("ok");
});

app.ws("/*", {
  compression: 0,
  maxPayloadLength: 16 * 1024 * 1024,
  idleTimeout: 60,

  open: (ws, req) => {
    try {
      const path = req.getUrl().slice(1);
      const decoded = Buffer.from(path, "base64").toString("utf8");
      const [host, port] = decoded.split(":");
      console.log(`New WS → TCP ${host}:${port}`);

      const socket = net.createConnection({ host, port: parseInt(port, 10) });
      ws.tcp = socket;

      socket.on("data", (chunk) => ws.send(chunk, true));
      socket.on("close", () => ws.end(1000, "TCP closed"));
      socket.on("error", (err) => {
        console.error("TCP error:", err.message);
        ws.end(1011, err.message);
      });
    } catch {
      ws.end(1011, "Invalid address");
    }
  },

  message: (ws, message, isBinary) => {
    if (ws.tcp && !ws.tcp.destroyed) ws.tcp.write(Buffer.from(message));
  },

  close: (ws) => {
    if (ws.tcp && !ws.tcp.destroyed) ws.tcp.destroy();
  },
});

app.listen(PORT, (token) => {
  if (token) console.log(`✅ Listening on ${PORT}`);
  else console.error(`❌ Failed to listen on ${PORT}`);
});
