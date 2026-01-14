package main

import (
    "context"
    "encoding/base64"
    "fmt"
    "io"
    "log"
    "net"
    "net/http"
    "sync"
    "sync/atomic"
    "time"

    "github.com/gorilla/websocket"
)

const (
    readBufferSize  = 8192
    writeBufferSize = 8192
    
    handshakeTimeout = 10 * time.Second
    readTimeout      = 300 * time.Second
    writeTimeout     = 30 * time.Second
    pingInterval     = 60 * time.Second
    
    maxMessageSize = 1024 * 1024
    
    // ✅ NEW: Connection limits
    maxConnections = 1000
    maxTCPDialTime = 5 * time.Second  // Reduce from 10s
)

type ProxyServer struct {
    upgrader    websocket.Upgrader
    connPool    sync.Map
    wg          sync.WaitGroup
    connCounter int64
    
    // ✅ NEW: Semaphore for connection limiting
    connSemaphore chan struct{}
}

func NewProxyServer() *ProxyServer {
    return &ProxyServer{
        upgrader:  websocket.Upgrader{
            ReadBufferSize:   readBufferSize,
            WriteBufferSize:  writeBufferSize,
            HandshakeTimeout: handshakeTimeout,
            CheckOrigin:  func(r *http.Request) bool {
                return true
            },
        },
        connSemaphore: make(chan struct{}, maxConnections),
    }
}

func (s *ProxyServer) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
    // ✅ FIX 1: Check capacity BEFORE upgrade
    select {
    case s.connSemaphore <- struct{}{}:
        defer func() { <-s.connSemaphore }()
    default:
        http.Error(w, "Server at capacity", http.StatusServiceUnavailable)
        log.Printf("503:  Rejected connection, current:  %d", atomic.LoadInt64(&s.connCounter))
        return
    }

    // Extract and decode address
    encodedAddr := r.URL.Path[1:]
    if encodedAddr == "" {
        http.Error(w, "Missing TCP address", http.StatusBadRequest)
        return
    }

    tcpAddr, err := base64.URLEncoding.DecodeString(encodedAddr)
    if err != nil {
        http.Error(w, "Invalid BASE64 address", http.StatusBadRequest)
        return
    }

    // ✅ FIX 2: Pre-validate TCP address (quick)
    if _, _, err := net.SplitHostPort(string(tcpAddr)); err != nil {
        http.Error(w, "Invalid TCP address format", http.StatusBadRequest)
        return
    }

    // Upgrade WebSocket
    ws, err := s.upgrader.Upgrade(w, r, nil)
    if err != nil {
        log.Printf("WebSocket upgrade error: %v", err)
        return
    }

    ws.SetReadLimit(maxMessageSize)

    // ✅ FIX 3: Dial TCP with SHORTER timeout
    dialCtx, dialCancel := context.WithTimeout(context.Background(), maxTCPDialTime)
    defer dialCancel()

    var d net.Dialer
    tcpConn, err := d. DialContext(dialCtx, "tcp", string(tcpAddr))
    if err != nil {
        log.Printf("TCP connection error to %s: %v", string(tcpAddr), err)
        // ✅ Send error message to client
        errMsg := fmt.Sprintf(`{"error":"Cannot connect to backend %s: %v"}`, string(tcpAddr), err)
        ws.WriteMessage(websocket. TextMessage, []byte(errMsg))
        ws.Close()
        return
    }

    // Configure TCP connection
    if tcpConn, ok := tcpConn.(*net.TCPConn); ok {
        tcpConn.SetNoDelay(true)
        tcpConn.SetKeepAlive(true)
        tcpConn.SetKeepAlivePeriod(30 * time.Second)
    }

    // Create connection context
    ctx, cancel := context.WithCancel(context.Background())
    conn := &Connection{
        ws:          ws,
        tcp:         tcpConn,
        ctx:         ctx,
        cancel:      cancel,
        ready:       make(chan struct{}),
        connectedAt: time.Now(),
        tcpAddr:     string(tcpAddr),
    }

    // Track connection
    connID := fmt.Sprintf("%p", conn)
    s.connPool.Store(connID, conn)

    currentCount := atomic.AddInt64(&s.connCounter, 1)
    log.Printf("New connection: WS %s -> TCP %s [Online: %d/%d]", 
        ws.RemoteAddr(), string(tcpAddr), currentCount, maxConnections)

    close(conn.ready)

    // Start bidirectional proxy
    s.wg.Add(1)
    go func() {
        defer s.wg.Done()
        conn.proxy()
        s.connPool.Delete(connID)
        
        currentCount := atomic.AddInt64(&s.connCounter, -1)
        log.Printf("Connection closed: WS %s -> TCP %s [Online: %d/%d]", 
            ws. RemoteAddr(), string(tcpAddr), currentCount, maxConnections)
    }()
}

// Connection struct và methods giữ nguyên nhưng fix bugs đã chỉ ra trước đó
type Connection struct {
    ws          *websocket.Conn
    tcp         net.Conn
    ctx         context.Context
    cancel      context. CancelFunc
    closedMu    sync. Mutex
    closed      bool
    ready       chan struct{}
    connectedAt time. Time
    tcpAddr     string
}

func (c *Connection) proxy() {
    defer c.cleanup()

    // ✅ Setup pong handler BEFORE goroutines
    c.ws.SetPongHandler(func(string) error {
        c.ws.SetReadDeadline(time.Now().Add(readTimeout))
        return nil
    })

    var wg sync.WaitGroup
    wg.Add(3)

    go func() {
        defer wg. Done()
        c.wsToTCP()
    }()

    go func() {
        defer wg.Done()
        c.tcpToWS()
    }()

    go func() {
        defer wg.Done()
        c.keepAlive()
    }()

    wg.Wait()
}

func (c *Connection) wsToTCP() {
    defer c.cancel()

    select {
    case <-c. ready:
    case <-c. ctx.Done():
        return
    }

    for {
        select {
        case <-c. ctx.Done():
            return
        default: 
        }

        c.ws.SetReadDeadline(time.Now().Add(readTimeout))
        messageType, message, err := c.ws.ReadMessage()
        if err != nil {
            if websocket.IsCloseError(err, 
                websocket.CloseNormalClosure, 
                websocket.CloseGoingAway,
                websocket.CloseAbnormalClosure) {
                return
            }
            if websocket.IsUnexpectedCloseError(err) {
                log.Printf("WS unexpected close from %s: %v", c. ws.RemoteAddr(), err)
                return
            }
            log.Printf("WS read error from %s: %v", c. ws.RemoteAddr(), err)
            return
        }

        if messageType != websocket.TextMessage {
            log.Printf("Warning:  Received non-text message type: %d", messageType)
            continue
        }

        // ✅ FIX: Copy before modifying
        if len(message) > 0 && message[len(message)-1] != '\n' {
            newMsg := make([]byte, len(message), len(message)+1)
            copy(newMsg, message)
            message = append(newMsg, '\n')
        }

        c.tcp.SetWriteDeadline(time.Now().Add(writeTimeout))
        _, err = c.tcp.Write(message)
        if err != nil {
            log.Printf("TCP write error to %s: %v", c.tcpAddr, err)
            return
        }
    }
}

func (c *Connection) tcpToWS() {
    defer c.cancel()

    select {
    case <-c. ready:
    case <-c.ctx.Done():
        return
    }

    buffer := make([]byte, readBufferSize)

    for {
        select {
        case <-c.ctx. Done():
            return
        default:
        }

        c. tcp.SetReadDeadline(time.Now().Add(readTimeout))
        n, err := c.tcp.Read(buffer)
        if err != nil {
            select {
            case <-c. ctx.Done():
                return
            default:
            }
            
            // ✅ Timeout được auto-reset bởi SetReadDeadline ở đầu loop
            if netErr, ok := err.(net. Error); ok && netErr.Timeout() {
                continue
            }
            
            if err != io.EOF {
                log. Printf("TCP read error from %s: %v", c.tcpAddr, err)
            }
            return
        }

        if n > 0 {
            c. ws.SetWriteDeadline(time.Now().Add(writeTimeout))
            err = c.ws.WriteMessage(websocket.TextMessage, buffer[: n])
            if err != nil {
                if websocket.IsCloseError(err, 
                    websocket.CloseNormalClosure, 
                    websocket.CloseGoingAway,
                    websocket.CloseAbnormalClosure) {
                    return
                }
                log.Printf("WS write error:  %v", err)
                return
            }
        }
    }
}

func (c *Connection) keepAlive() {
    ticker := time.NewTicker(pingInterval)
    defer ticker.Stop()

    for {
        select {
        case <-c. ctx.Done():
            return
        case <-ticker.C: 
            c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
            if err := c.ws.WriteMessage(websocket.PingMessage, nil); err != nil {
                return
            }
        }
    }
}

func (c *Connection) cleanup() {
    c.closedMu.Lock()
    defer c.closedMu. Unlock()

    if c.closed {
        return
    }
    c.closed = true

    c.cancel()

    if c.ws != nil {
        closeMsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
        c.ws.WriteControl(websocket.CloseMessage, closeMsg, time.Now().Add(time.Second))
        c.ws.Close()
    }

    if c.tcp != nil {
        c.tcp.Close()
    }
}

func (s *ProxyServer) Shutdown(ctx context.Context) error {
    log.Println("Shutting down proxy server...")

    s.connPool.Range(func(key, value interface{}) bool {
        if conn, ok := value.(*Connection); ok {
            conn.cleanup()
        }
        return true
    })

    done := make(chan struct{})
    go func() {
        s.wg.Wait()
        close(done)
    }()

    select {
    case <-done:
        log.Println("All connections closed")
    case <-ctx.Done():
        log.Println("Shutdown timeout")
    }

    return nil
}

func main() {
    proxy := NewProxyServer()

    mux := http.NewServeMux()
    mux.HandleFunc("/", proxy.HandleWebSocket)
    
    // ✅ ADD: Health check endpoint
    mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
        current := atomic.LoadInt64(&proxy.connCounter)
        status := "healthy"
        statusCode := http.StatusOK
        
        if current >= maxConnections {
            status = "at_capacity"
            statusCode = http.StatusServiceUnavailable
        }
        
        w.WriteHeader(statusCode)
        fmt.Fprintf(w, `{"status": "%s","connections":%d,"max":%d}`, status, current, maxConnections)
    })

    server := &http.Server{
        Addr:         "0.0.0.0:8000",
        Handler:      mux,
        ReadTimeout:  30 * time.Second,
        WriteTimeout: 30 * time.Second,
        IdleTimeout:  120 * time.Second,
    }

    log.Printf("Mining proxy server starting on %s", server.Addr)
    log.Printf("Max connections: %d", maxConnections)
    log.Printf("Health check:  http://127.0.0.0:8000/health")
    
    if err := server.ListenAndServe(); err != nil {
        log.Fatal(err)
    }
}
