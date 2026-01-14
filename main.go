package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
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
	
	maxConnections = 100000
	maxTCPDialTime = 5 * time.Second
)

type ProxyServer struct {
	upgrader      websocket.Upgrader
	connPool      sync.Map
	wg            sync.WaitGroup
	connCounter   int64
	connSemaphore chan struct{}
}

type Connection struct {
	ws          *websocket.Conn
	tcp         net.Conn
	ctx         context.Context
	cancel      context.CancelFunc
	closedMu    sync.Mutex
	closed      bool
	ready       chan struct{}
	connectedAt time.Time
	tcpAddr     string
	writeMu     sync.Mutex // Protect concurrent writes to WS
}

func NewProxyServer() *ProxyServer {
	return &ProxyServer{
		upgrader: websocket.Upgrader{
			ReadBufferSize:   readBufferSize,
			WriteBufferSize:  writeBufferSize,
			HandshakeTimeout: handshakeTimeout,
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
		connSemaphore: make(chan struct{}, maxConnections),
	}
}

func (s *ProxyServer) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Check capacity
	select {
	case s.connSemaphore <- struct{}{}:
		defer func() { <-s.connSemaphore }()
	default:
		http.Error(w, "Server at capacity", http.StatusServiceUnavailable)
		log.Printf("503: Rejected connection, current: %d", atomic.LoadInt64(&s.connCounter))
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

	// Pre-validate TCP address
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

	// Dial TCP with timeout
	dialCtx, dialCancel := context.WithTimeout(context.Background(), maxTCPDialTime)
	defer dialCancel()

	var d net.Dialer
	tcpConn, err := d.DialContext(dialCtx, "tcp", string(tcpAddr))
	if err != nil {
		log.Printf("TCP connection error to %s: %v", string(tcpAddr), err)
		errMsg := fmt.Sprintf(`{"error":"Cannot connect to backend %s: %v"}`, string(tcpAddr), err)
		ws.WriteMessage(websocket.TextMessage, []byte(errMsg))
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
			ws.RemoteAddr(), string(tcpAddr), currentCount, maxConnections)
	}()
}

func (c *Connection) proxy() {
	defer c.cleanup()

	// Setup pong handler
	c.ws.SetPongHandler(func(string) error {
		c.ws.SetReadDeadline(time.Now().Add(readTimeout))
		return nil
	})

	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
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
	case <-c.ready:
	case <-c.ctx.Done():
		return
	}

	for {
		select {
		case <-c.ctx.Done():
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
			if !websocket.IsUnexpectedCloseError(err) {
				return
			}
			return
		}

		if messageType != websocket.TextMessage {
			continue
		}

		// Add newline if needed
		if len(message) > 0 && message[len(message)-1] != '\n' {
			message = append(message, '\n')
		}

		// Check if connection still alive before writing
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		c.tcp.SetWriteDeadline(time.Now().Add(writeTimeout))
		_, err = c.tcp.Write(message)
		if err != nil {
			// Don't log if connection already closing
			select {
			case <-c.ctx.Done():
				return
			default:
				if !isBrokenPipe(err) {
					log.Printf("TCP write error to %s: %v", c.tcpAddr, err)
				}
			}
			return
		}
	}
}

func (c *Connection) tcpToWS() {
	defer c.cancel()

	select {
	case <-c.ready:
	case <-c.ctx.Done():
		return
	}

	buffer := make([]byte, readBufferSize)

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		c.tcp.SetReadDeadline(time.Now().Add(readTimeout))
		n, err := c.tcp.Read(buffer)
		if err != nil {
			select {
			case <-c.ctx.Done():
				return
			default:
			}
			
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			
			if err != io.EOF && !isBrokenPipe(err) {
				log.Printf("TCP read error from %s: %v", c.tcpAddr, err)
			}
			return
		}

		if n > 0 {
			// Check if connection still alive
			select {
			case <-c.ctx.Done():
				return
			default:
			}

			// Thread-safe write to WebSocket
			c.writeMu.Lock()
			c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
			err = c.ws.WriteMessage(websocket.TextMessage, buffer[:n])
			c.writeMu.Unlock()

			if err != nil {
				if websocket.IsCloseError(err, 
					websocket.CloseNormalClosure, 
					websocket.CloseGoingAway,
					websocket.CloseAbnormalClosure) {
					return
				}
				// Don't log broken pipe - client disconnected
				if !isBrokenPipe(err) {
					log.Printf("WS write error to %s: %v", c.ws.RemoteAddr(), err)
				}
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
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			// Thread-safe write
			c.writeMu.Lock()
			c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
			err := c.ws.WriteMessage(websocket.PingMessage, nil)
			c.writeMu.Unlock()

			if err != nil {
				// Silently fail on broken pipe - connection is closing
				if !isBrokenPipe(err) {
					log.Printf("Ping failed to %s: %v", c.ws.RemoteAddr(), err)
				}
				return
			}
		}
	}
}

func (c *Connection) cleanup() {
	c.closedMu.Lock()
	defer c.closedMu.Unlock()

	if c.closed {
		return
	}
	c.closed = true

	// Cancel context first to stop all goroutines
	c.cancel()

	// Give goroutines time to exit
	time.Sleep(100 * time.Millisecond)

	// Close WebSocket
	if c.ws != nil {
		c.writeMu.Lock()
		closeMsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
		c.ws.WriteControl(websocket.CloseMessage, closeMsg, time.Now().Add(time.Second))
		c.ws.Close()
		c.writeMu.Unlock()
	}

	// Close TCP
	if c.tcp != nil {
		c.tcp.Close()
	}
}

// Helper function to detect broken pipe errors
func isBrokenPipe(err error) bool {
	if err == nil {
		return false
	}
	
	// Check for broken pipe error
	if errors.Is(err, syscall.EPIPE) {
		return true
	}
	
	// Check for connection reset
	if errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	
	// Check error message
	errStr := err.Error()
	return contains(errStr, "broken pipe") || 
	       contains(errStr, "connection reset") ||
	       contains(errStr, "use of closed network connection")
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && 
	       (s == substr || len(s) > len(substr) && 
	        (s[:len(substr)] == substr || s[len(s)-len(substr):] == substr || 
	         findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
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

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intValue, err := strconv.Atoi(value); err == nil {
			return intValue
		}
	}
	return defaultValue
}

func main() {
	// Get configuration from environment variables
	port := getEnv("PORT", "8000")
	host := getEnv("HOST", "0.0.0.0")
	maxConns := getEnvInt("MAX_CONNECTIONS", maxConnections)
	
	proxy := NewProxyServer()

	mux := http.NewServeMux()
	mux.HandleFunc("/", proxy.HandleWebSocket)
	
	// Health check endpoint
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		current := atomic.LoadInt64(&proxy.connCounter)
		status := "healthy"
		statusCode := http.StatusOK
		
		if current >= int64(maxConns) {
			status = "at_capacity"
			statusCode = http.StatusServiceUnavailable
		}
		
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		fmt.Fprintf(w, `{"status":"%s","connections":%d,"max":%d}`, status, current, maxConns)
	})

	addr := fmt.Sprintf("%s:%s", host, port)
	server := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	log.Printf("Mining proxy server starting on %s", addr)
	log.Printf("Max connections: %d", maxConns)
	log.Printf("Health check: http://%s:%s/health", host, port)
	log.Printf("WebSocket: ws://%s:%s/BASE64_ENCODED_ADDRESS", host, port)
	
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
