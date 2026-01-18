package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
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

	defaultMaxConnections    = 100000
	defaultMaxConnsPerIP     = 100
	defaultMaxTCPDialTime    = 5 * time.Second
	defaultShutdownTimeout   = 30 * time.Second
	defaultCleanupGraceTime  = 200 * time.Millisecond

	// Circuit breaker settings
	circuitBreakerThreshold = 10
	circuitBreakerTimeout   = 60 * time.Second
)

// Buffer pool for memory efficiency
var bufferPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, readBufferSize)
		return &b
	},
}

// Config holds server configuration
type Config struct {
	Host              string
	Port              string
	MaxConnections    int
	MaxConnsPerIP     int
	MaxTCPDialTime    time.Duration
	ShutdownTimeout   time.Duration
	EnableCompression bool
	CleanupGraceTime  time.Duration
}

// LoadConfig loads configuration from environment
func LoadConfig() (*Config, error) {
	cfg := &Config{
		Host:              getEnv("HOST", "0.0.0.0"),
		Port:              getEnv("PORT", "8000"),
		MaxConnections:    getEnvInt("MAX_CONNECTIONS", defaultMaxConnections),
		MaxConnsPerIP:     getEnvInt("MAX_CONNS_PER_IP", defaultMaxConnsPerIP),
		MaxTCPDialTime:    getEnvDuration("MAX_TCP_DIAL_TIME", defaultMaxTCPDialTime),
		ShutdownTimeout:   getEnvDuration("SHUTDOWN_TIMEOUT", defaultShutdownTimeout),
		EnableCompression: getEnvBool("ENABLE_COMPRESSION", true),
		CleanupGraceTime:  getEnvDuration("CLEANUP_GRACE_TIME", defaultCleanupGraceTime),
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return cfg, nil
}

func (c *Config) Validate() error {
	if c.MaxConnections <= 0 {
		return errors.New("MAX_CONNECTIONS must be positive")
	}
	if c.MaxConnsPerIP <= 0 {
		return errors.New("MAX_CONNS_PER_IP must be positive")
	}
	if c.MaxTCPDialTime <= 0 {
		return errors.New("MAX_TCP_DIAL_TIME must be positive")
	}
	if c.Port == "" {
		return errors.New("PORT cannot be empty")
	}
	return nil
}

// IPLimiter tracks connections per IP
type IPLimiter struct {
	mu      sync.RWMutex
	clients map[string]int
	maxPer  int
}

func NewIPLimiter(maxPerIP int) *IPLimiter {
	return &IPLimiter{
		clients: make(map[string]int),
		maxPer:  maxPerIP,
	}
}

func (l *IPLimiter) Acquire(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.clients[ip] >= l.maxPer {
		return false
	}
	l.clients[ip]++
	return true
}

func (l *IPLimiter) Release(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.clients[ip] > 0 {
		l.clients[ip]--
		if l.clients[ip] == 0 {
			delete(l.clients, ip)
		}
	}
}

// CircuitBreaker prevents cascading failures
type CircuitBreaker struct {
	mu          sync.RWMutex
	failures    map[string]int
	lastFailure map[string]time.Time
	threshold   int
	timeout     time.Duration
}

func NewCircuitBreaker(threshold int, timeout time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		failures:    make(map[string]int),
		lastFailure: make(map[string]time.Time),
		threshold:   threshold,
		timeout:     timeout,
	}
}

func (cb *CircuitBreaker) IsOpen(target string) bool {
	cb.mu.RLock()
	failures := cb.failures[target]
	lastFail := cb.lastFailure[target]
	cb.mu.RUnlock()

	if failures >= cb.threshold {
		if time.Since(lastFail) < cb.timeout {
			return true
		}
		// Reset after timeout
		cb.mu.Lock()
		delete(cb.failures, target)
		delete(cb.lastFailure, target)
		cb.mu.Unlock()
	}

	return false
}

func (cb *CircuitBreaker) RecordSuccess(target string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	delete(cb.failures, target)
	delete(cb.lastFailure, target)
}

func (cb *CircuitBreaker) RecordFailure(target string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures[target]++
	cb.lastFailure[target] = time.Now()
}

// ProxyServer manages WebSocket connections
type ProxyServer struct {
	upgrader       websocket.Upgrader
	connPool       sync.Map
	wg             sync.WaitGroup
	connCounter    int64
	connSemaphore  chan struct{}
	ipLimiter      *IPLimiter
	circuitBreaker *CircuitBreaker
	config         *Config
	shutdownOnce   sync.Once
}

// Connection represents a proxied connection
type Connection struct {
	id          string
	ws          *websocket.Conn
	tcp         net.Conn
	ctx         context.Context
	cancel      context.CancelFunc
	closedMu    sync.Mutex
	closed      bool
	ready       chan struct{}
	connectedAt time.Time
	tcpAddr     string
	clientIP    string
	writeMu     sync.Mutex
	wg          sync.WaitGroup
}

func NewProxyServer(cfg *Config) *ProxyServer {
	return &ProxyServer{
		upgrader: websocket.Upgrader{
			ReadBufferSize:    readBufferSize,
			WriteBufferSize:   writeBufferSize,
			HandshakeTimeout:  handshakeTimeout,
			EnableCompression: cfg.EnableCompression,
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
		connSemaphore:  make(chan struct{}, cfg.MaxConnections),
		ipLimiter:      NewIPLimiter(cfg.MaxConnsPerIP),
		circuitBreaker: NewCircuitBreaker(circuitBreakerThreshold, circuitBreakerTimeout),
		config:         cfg,
	}
}

func (s *ProxyServer) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Panic recovery
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("PANIC in HandleWebSocket: %v\n%s", rec, debug.Stack())
		}
	}()

	clientIP := getClientIP(r)

	// Check IP rate limit
	if !s.ipLimiter.Acquire(clientIP) {
		http.Error(w, "Too many connections from your IP", http.StatusTooManyRequests)
		log.Printf("Rejected connection from %s: IP limit reached", clientIP)
		return
	}

	// Check global capacity
	select {
	case s.connSemaphore <- struct{}{}:
	default:
		s.ipLimiter.Release(clientIP)
		http.Error(w, "Server at capacity", http.StatusServiceUnavailable)
		log.Printf("Rejected connection: server at capacity (%d/%d)",
			atomic.LoadInt64(&s.connCounter), s.config.MaxConnections)
		return
	}

	// Extract and decode address
	encodedAddr := r.URL.Path[1:]
	if encodedAddr == "" {
		s.releaseResources(clientIP)
		http.Error(w, "Missing TCP address", http.StatusBadRequest)
		return
	}

	tcpAddrBytes, err := base64.URLEncoding.DecodeString(encodedAddr)
	if err != nil {
		s.releaseResources(clientIP)
		http.Error(w, "Invalid BASE64 address", http.StatusBadRequest)
		return
	}

	tcpAddr := string(tcpAddrBytes)

	// Pre-validate TCP address
	if _, _, err := net.SplitHostPort(tcpAddr); err != nil {
		s.releaseResources(clientIP)
		http.Error(w, "Invalid TCP address format", http.StatusBadRequest)
		return
	}

	// Check circuit breaker
	if s.circuitBreaker.IsOpen(tcpAddr) {
		s.releaseResources(clientIP)
		http.Error(w, "Backend temporarily unavailable", http.StatusServiceUnavailable)
		log.Printf("Circuit breaker open for %s", tcpAddr)
		return
	}

	// Upgrade WebSocket
	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.releaseResources(clientIP)
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}
	ws.SetReadLimit(maxMessageSize)

	// Dial TCP with timeout
	dialCtx, dialCancel := context.WithTimeout(context.Background(), s.config.MaxTCPDialTime)
	defer dialCancel()

	var d net.Dialer
	tcpConn, err := d.DialContext(dialCtx, "tcp", tcpAddr)

	if err != nil {
		s.releaseResources(clientIP)
		s.circuitBreaker.RecordFailure(tcpAddr)

		log.Printf("TCP dial failed to %s: %v", tcpAddr, err)

		errMsg := map[string]string{
			"error": fmt.Sprintf("Cannot connect to backend %s", tcpAddr),
		}
		if jsonErr, _ := json.Marshal(errMsg); jsonErr != nil {
			ws.WriteMessage(websocket.TextMessage, jsonErr)
		}
		ws.Close()
		return
	}

	s.circuitBreaker.RecordSuccess(tcpAddr)

	// Configure TCP connection
	if tcpConn, ok := tcpConn.(*net.TCPConn); ok {
		tcpConn.SetNoDelay(true)
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(30 * time.Second)
	}

	// Create connection
	ctx, cancel := context.WithCancel(context.Background())
	connID := fmt.Sprintf("%p-%d", ws, time.Now().UnixNano())

	conn := &Connection{
		id:          connID,
		ws:          ws,
		tcp:         tcpConn,
		ctx:         ctx,
		cancel:      cancel,
		ready:       make(chan struct{}),
		connectedAt: time.Now(),
		tcpAddr:     tcpAddr,
		clientIP:    clientIP,
	}

	// Track connection
	s.connPool.Store(connID, conn)
	currentCount := atomic.AddInt64(&s.connCounter, 1)

	log.Printf("New connection: %s -> %s [%d/%d]",
		clientIP, tcpAddr, currentCount, s.config.MaxConnections)

	close(conn.ready)

	// Start proxy in background
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.releaseResources(clientIP)

		conn.proxy()

		duration := time.Since(conn.connectedAt)

		s.connPool.Delete(connID)
		currentCount := atomic.AddInt64(&s.connCounter, -1)

		log.Printf("Connection closed: %s -> %s [Duration: %v] [%d/%d]",
			clientIP, tcpAddr, duration, currentCount, s.config.MaxConnections)
	}()
}

func (s *ProxyServer) releaseResources(clientIP string) {
	s.ipLimiter.Release(clientIP)
	<-s.connSemaphore
}

func (c *Connection) proxy() {
	defer c.cleanup()

	// Setup pong handler
	c.ws.SetPongHandler(func(string) error {
		c.ws.SetReadDeadline(time.Now().Add(readTimeout))
		return nil
	})

	c.wg.Add(3)

	go func() {
		defer c.wg.Done()
		c.wsToTCP()
	}()

	go func() {
		defer c.wg.Done()
		c.tcpToWS()
	}()

	go func() {
		defer c.wg.Done()
		c.keepAlive()
	}()

	c.wg.Wait()
}

func (c *Connection) wsToTCP() {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("PANIC in wsToTCP [%s]: %v", c.id, rec)
		}
		c.cancel()
	}()

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
			if !isExpectedCloseError(err) {
				log.Printf("WS read error [%s]: %v", c.id, err)
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

		select {
		case <-c.ctx.Done():
			return
		default:
		}

		c.tcp.SetWriteDeadline(time.Now().Add(writeTimeout))
		_, err = c.tcp.Write(message)
		if err != nil {
			if !isBrokenPipe(err) {
				log.Printf("TCP write error [%s]: %v", c.id, err)
			}
			return
		}
	}
}

func (c *Connection) tcpToWS() {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("PANIC in tcpToWS [%s]: %v", c.id, rec)
		}
		c.cancel()
	}()

	select {
	case <-c.ready:
	case <-c.ctx.Done():
		return
	}

	// Get buffer from pool
	bufPtr := bufferPool.Get().(*[]byte)
	buffer := *bufPtr
	defer bufferPool.Put(bufPtr)

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		c.tcp.SetReadDeadline(time.Now().Add(readTimeout))
		n, err := c.tcp.Read(buffer)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}

			if err != io.EOF && !isBrokenPipe(err) {
				log.Printf("TCP read error [%s]: %v", c.id, err)
			}
			return
		}

		if n > 0 {
			select {
			case <-c.ctx.Done():
				return
			default:
			}

			c.writeMu.Lock()
			c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
			err = c.ws.WriteMessage(websocket.TextMessage, buffer[:n])
			c.writeMu.Unlock()

			if err != nil {
				if !isExpectedCloseError(err) {
					log.Printf("WS write error [%s]: %v", c.id, err)
				}
				return
			}
		}
	}
}

func (c *Connection) keepAlive() {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("PANIC in keepAlive [%s]: %v", c.id, rec)
		}
	}()

	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.writeMu.Lock()
			c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
			err := c.ws.WriteMessage(websocket.PingMessage, nil)
			c.writeMu.Unlock()

			if err != nil {
				if !isBrokenPipe(err) {
					log.Printf("Ping failed [%s]: %v", c.id, err)
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

	// Cancel context
	c.cancel()

	// Wait for goroutines with timeout
	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		log.Printf("Cleanup timeout for connection %s", c.id)
	}

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

func (s *ProxyServer) Shutdown(ctx context.Context) error {
	var shutdownErr error

	s.shutdownOnce.Do(func() {
		log.Println("Shutting down proxy server...")

		// Close all connections
		s.connPool.Range(func(key, value interface{}) bool {
			if conn, ok := value.(*Connection); ok {
				conn.cleanup()
			}
			return true
		})

		// Wait for connections with timeout
		done := make(chan struct{})
		go func() {
			s.wg.Wait()
			close(done)
		}()

		select {
		case <-done:
			log.Println("All connections closed gracefully")
		case <-ctx.Done():
			log.Println("Shutdown timeout, forcing close")
			shutdownErr = ctx.Err()
		}
	})

	return shutdownErr
}

// Health check handlers
func (s *ProxyServer) healthHandler(w http.ResponseWriter, r *http.Request) {
	current := atomic.LoadInt64(&s.connCounter)

	health := map[string]interface{}{
		"status":      "healthy",
		"connections": current,
		"max":         s.config.MaxConnections,
		"timestamp":   time.Now().Unix(),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(health)
}

func (s *ProxyServer) readinessHandler(w http.ResponseWriter, r *http.Request) {
	current := atomic.LoadInt64(&s.connCounter)

	if current >= int64(s.config.MaxConnections) {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{
			"status": "unavailable",
			"reason": "at capacity",
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
}

func (s *ProxyServer) livenessHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "alive"})
}

// Helper functions
func isExpectedCloseError(err error) bool {
	return websocket.IsCloseError(err,
		websocket.CloseNormalClosure,
		websocket.CloseGoingAway,
		websocket.CloseAbnormalClosure,
	) || websocket.IsUnexpectedCloseError(err)
}

func isBrokenPipe(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNABORTED)
}

func getClientIP(r *http.Request) string {
	// Check X-Forwarded-For header
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return xff
	}
	// Check X-Real-IP header
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}
	// Fallback to RemoteAddr
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
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

func getEnvDuration(key string, defaultValue time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if duration, err := time.ParseDuration(value); err == nil {
			return duration
		}
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		if boolValue, err := strconv.ParseBool(value); err == nil {
			return boolValue
		}
	}
	return defaultValue
}

func main() {
	// Load configuration
	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("Configuration error: %v", err)
	}

	// Create proxy server
	proxy := NewProxyServer(cfg)

	// Setup HTTP server
	mux := http.NewServeMux()
	mux.HandleFunc("/", proxy.HandleWebSocket)
	mux.HandleFunc("/health", proxy.healthHandler)
	mux.HandleFunc("/healthz", proxy.livenessHandler)
	mux.HandleFunc("/ready", proxy.readinessHandler)

	addr := fmt.Sprintf("%s:%s", cfg.Host, cfg.Port)
	server := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Setup signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start server
	go func() {
		log.Printf("Mining proxy server starting on %s", addr)
		log.Printf("Configuration:")
		log.Printf("  - Max connections: %d", cfg.MaxConnections)
		log.Printf("  - Max connections per IP: %d", cfg.MaxConnsPerIP)
		log.Printf("  - WebSocket compression: %v", cfg.EnableCompression)
		log.Printf("  - TCP dial timeout: %v", cfg.MaxTCPDialTime)
		log.Printf("")
		log.Printf("Endpoints:")
		log.Printf("  - WebSocket: ws://%s/BASE64_ENCODED_ADDRESS", addr)
		log.Printf("  - Health: http://%s/health", addr)
		log.Printf("  - Liveness: http://%s/healthz", addr)
		log.Printf("  - Readiness: http://%s/ready", addr)

		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	// Wait for shutdown signal
	<-sigChan
	log.Println("Shutdown signal received")

	// Graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer shutdownCancel()

	// Shutdown HTTP server
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}

	// Shutdown proxy connections
	if err := proxy.Shutdown(shutdownCtx); err != nil {
		log.Printf("Proxy shutdown error: %v", err)
	}

	log.Println("Server stopped gracefully")
}
