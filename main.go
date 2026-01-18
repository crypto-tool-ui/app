package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
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

// Metrics
var (
	activeConnections = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "wsproxy_active_connections",
		Help: "Number of active WebSocket connections",
	})

	totalConnections = promauto.NewCounter(prometheus.CounterOpts{
		Name: "wsproxy_total_connections",
		Help: "Total number of WebSocket connections",
	})

	rejectedConnections = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "wsproxy_rejected_connections_total",
		Help: "Total number of rejected connections",
	}, []string{"reason"})

	connectionDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "wsproxy_connection_duration_seconds",
		Help:    "Duration of WebSocket connections",
		Buckets: prometheus.ExponentialBuckets(1, 2, 10),
	})

	bytesTransferred = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "wsproxy_bytes_transferred_total",
		Help: "Total bytes transferred",
	}, []string{"direction"})

	tcpDialDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "wsproxy_tcp_dial_duration_seconds",
		Help:    "Duration of TCP dial operations",
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 10),
	})

	tcpDialErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "wsproxy_tcp_dial_errors_total",
		Help: "Total number of TCP dial errors",
	}, []string{"target"})

	messagesSent = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "wsproxy_messages_total",
		Help: "Total number of messages",
	}, []string{"direction"})

	errors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "wsproxy_errors_total",
		Help: "Total number of errors",
	}, []string{"type"})
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
	Host               string
	Port               string
	MaxConnections     int
	MaxConnsPerIP      int
	MaxTCPDialTime     time.Duration
	ShutdownTimeout    time.Duration
	EnableCompression  bool
	EnableMetrics      bool
	LogLevel           string
	CleanupGraceTime   time.Duration
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
		EnableMetrics:     getEnvBool("ENABLE_METRICS", true),
		LogLevel:          getEnv("LOG_LEVEL", "info"),
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
	mu           sync.RWMutex
	failures     map[string]int
	lastFailure  map[string]time.Time
	threshold    int
	timeout      time.Duration
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
	defer cb.mu.RUnlock()

	failures := cb.failures[target]
	lastFail := cb.lastFailure[target]

	if failures >= cb.threshold {
		if time.Since(lastFail) < cb.timeout {
			return true
		}
		// Reset after timeout
		cb.mu.RUnlock()
		cb.mu.Lock()
		delete(cb.failures, target)
		delete(cb.lastFailure, target)
		cb.mu.Unlock()
		cb.mu.RLock()
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
	upgrader        websocket.Upgrader
	connPool        sync.Map
	wg              sync.WaitGroup
	connCounter     int64
	connSemaphore   chan struct{}
	ipLimiter       *IPLimiter
	circuitBreaker  *CircuitBreaker
	logger          *zap.Logger
	config          *Config
	shutdownOnce    sync.Once
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
	logger      *zap.Logger
}

func NewProxyServer(cfg *Config, logger *zap.Logger) *ProxyServer {
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
		logger:         logger,
		config:         cfg,
	}
}

func (s *ProxyServer) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Panic recovery
	defer func() {
		if rec := recover(); rec != nil {
			s.logger.Error("panic in HandleWebSocket",
				zap.Any("panic", rec),
				zap.String("stack", string(debug.Stack())),
			)
			errors.Add("panic", 1)
		}
	}()

	clientIP := getClientIP(r)

	// Check IP rate limit
	if !s.ipLimiter.Acquire(clientIP) {
		rejectedConnections.WithLabelValues("ip_limit").Inc()
		http.Error(w, "Too many connections from your IP", http.StatusTooManyRequests)
		s.logger.Warn("rejected connection: IP limit",
			zap.String("ip", clientIP),
		)
		return
	}

	// Check global capacity
	select {
	case s.connSemaphore <- struct{}{}:
	default:
		s.ipLimiter.Release(clientIP)
		rejectedConnections.WithLabelValues("capacity").Inc()
		http.Error(w, "Server at capacity", http.StatusServiceUnavailable)
		s.logger.Warn("rejected connection: at capacity",
			zap.Int64("current", atomic.LoadInt64(&s.connCounter)),
			zap.Int("max", s.config.MaxConnections),
		)
		return
	}

	// Extract and decode address
	encodedAddr := r.URL.Path[1:]
	if encodedAddr == "" {
		s.releaseResources(clientIP)
		rejectedConnections.WithLabelValues("missing_address").Inc()
		http.Error(w, "Missing TCP address", http.StatusBadRequest)
		return
	}

	tcpAddrBytes, err := base64.URLEncoding.DecodeString(encodedAddr)
	if err != nil {
		s.releaseResources(clientIP)
		rejectedConnections.WithLabelValues("invalid_address").Inc()
		http.Error(w, "Invalid BASE64 address", http.StatusBadRequest)
		return
	}

	tcpAddr := string(tcpAddrBytes)

	// Pre-validate TCP address
	if _, _, err := net.SplitHostPort(tcpAddr); err != nil {
		s.releaseResources(clientIP)
		rejectedConnections.WithLabelValues("invalid_format").Inc()
		http.Error(w, "Invalid TCP address format", http.StatusBadRequest)
		return
	}

	// Check circuit breaker
	if s.circuitBreaker.IsOpen(tcpAddr) {
		s.releaseResources(clientIP)
		rejectedConnections.WithLabelValues("circuit_open").Inc()
		http.Error(w, "Backend temporarily unavailable", http.StatusServiceUnavailable)
		s.logger.Warn("circuit breaker open",
			zap.String("target", tcpAddr),
		)
		return
	}

	// Upgrade WebSocket
	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.releaseResources(clientIP)
		s.logger.Error("WebSocket upgrade failed",
			zap.Error(err),
			zap.String("client", clientIP),
		)
		return
	}
	ws.SetReadLimit(maxMessageSize)

	// Dial TCP with timeout
	dialStart := time.Now()
	dialCtx, dialCancel := context.WithTimeout(context.Background(), s.config.MaxTCPDialTime)
	defer dialCancel()

	var d net.Dialer
	tcpConn, err := d.DialContext(dialCtx, "tcp", tcpAddr)
	tcpDialDuration.Observe(time.Since(dialStart).Seconds())

	if err != nil {
		s.releaseResources(clientIP)
		s.circuitBreaker.RecordFailure(tcpAddr)
		tcpDialErrors.WithLabelValues(tcpAddr).Inc()
		
		s.logger.Error("TCP dial failed",
			zap.Error(err),
			zap.String("target", tcpAddr),
			zap.String("client", clientIP),
		)
		
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
		logger: s.logger.With(
			zap.String("conn_id", connID),
			zap.String("client", clientIP),
			zap.String("target", tcpAddr),
		),
	}

	// Track connection
	s.connPool.Store(connID, conn)
	currentCount := atomic.AddInt64(&s.connCounter, 1)
	activeConnections.Set(float64(currentCount))
	totalConnections.Inc()

	conn.logger.Info("connection established",
		zap.Int64("total", currentCount),
		zap.Int("max", s.config.MaxConnections),
	)

	close(conn.ready)

	// Start proxy in background
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.releaseResources(clientIP)
		
		conn.proxy()
		
		duration := time.Since(conn.connectedAt).Seconds()
		connectionDuration.Observe(duration)
		
		s.connPool.Delete(connID)
		currentCount := atomic.AddInt64(&s.connCounter, -1)
		activeConnections.Set(float64(currentCount))
		
		conn.logger.Info("connection closed",
			zap.Float64("duration_sec", duration),
			zap.Int64("total", currentCount),
		)
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
			c.logger.Error("panic in wsToTCP",
				zap.Any("panic", rec),
				zap.String("stack", string(debug.Stack())),
			)
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
				c.logger.Debug("ws read error", zap.Error(err))
				errors.WithLabelValues("ws_read").Inc()
			}
			return
		}

		if messageType != websocket.TextMessage {
			continue
		}

		messagesSent.WithLabelValues("ws_to_tcp").Inc()

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
		n, err := c.tcp.Write(message)
		if err != nil {
			if !isBrokenPipe(err) {
				c.logger.Debug("tcp write error", zap.Error(err))
				errors.WithLabelValues("tcp_write").Inc()
			}
			return
		}
		
		bytesTransferred.WithLabelValues("ws_to_tcp").Add(float64(n))
	}
}

func (c *Connection) tcpToWS() {
	defer func() {
		if rec := recover(); rec != nil {
			c.logger.Error("panic in tcpToWS",
				zap.Any("panic", rec),
				zap.String("stack", string(debug.Stack())),
			)
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
				c.logger.Debug("tcp read error", zap.Error(err))
				errors.WithLabelValues("tcp_read").Inc()
			}
			return
		}

		if n > 0 {
			messagesSent.WithLabelValues("tcp_to_ws").Inc()
			bytesTransferred.WithLabelValues("tcp_to_ws").Add(float64(n))

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
					c.logger.Debug("ws write error", zap.Error(err))
					errors.WithLabelValues("ws_write").Inc()
				}
				return
			}
		}
	}
}

func (c *Connection) keepAlive() {
	defer func() {
		if rec := recover(); rec != nil {
			c.logger.Error("panic in keepAlive",
				zap.Any("panic", rec),
			)
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
					c.logger.Debug("ping failed", zap.Error(err))
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
	case <-time.After(c.logger.Core().Enabled(zapcore.DebugLevel) ? 500*time.Millisecond : 200*time.Millisecond):
		c.logger.Warn("cleanup timeout waiting for goroutines")
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
		s.logger.Info("shutting down proxy server")

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
			s.logger.Info("all connections closed gracefully")
		case <-ctx.Done():
			s.logger.Warn("shutdown timeout, forcing close")
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

func initLogger(level string) (*zap.Logger, error) {
	var zapLevel zapcore.Level
	if err := zapLevel.UnmarshalText([]byte(level)); err != nil {
		zapLevel = zapcore.InfoLevel
	}

	config := zap.NewProductionConfig()
	config.Level = zap.NewAtomicLevelAt(zapLevel)
	config.EncoderConfig.TimeKey = "timestamp"
	config.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder

	return config.Build()
}

func main() {
	// Load configuration
	cfg, err := LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Configuration error: %v\n", err)
		os.Exit(1)
	}

	// Initialize logger
	logger, err := initLogger(cfg.LogLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Logger initialization error: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync()

	// Create proxy server
	proxy := NewProxyServer(cfg, logger)

	// Setup HTTP server
	mux := http.NewServeMux()
	mux.HandleFunc("/", proxy.HandleWebSocket)
	mux.HandleFunc("/health", proxy.healthHandler)
	mux.HandleFunc("/healthz", proxy.livenessHandler)
	mux.HandleFunc("/ready", proxy.readinessHandler)

	// Metrics endpoint
	if cfg.EnableMetrics {
		mux.Handle("/metrics", promhttp.Handler())
	}

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
		logger.Info("server starting",
			zap.String("addr", addr),
			zap.Int("max_connections", cfg.MaxConnections),
			zap.Int("max_conns_per_ip", cfg.MaxConnsPerIP),
			zap.Bool("compression", cfg.EnableCompression),
			zap.Bool("metrics", cfg.EnableMetrics),
		)

		logger.Info("endpoints available",
			zap.String("websocket", fmt.Sprintf("ws://%s/BASE64_ENCODED_ADDRESS", addr)),
			zap.String("health", fmt.Sprintf("http://%s/health", addr)),
			zap.String("liveness", fmt.Sprintf("http://%s/healthz", addr)),
			zap.String("readiness", fmt.Sprintf("http://%s/ready", addr)),
			zap.String("metrics", fmt.Sprintf("http://%s/metrics", addr)),
		)

		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("server failed", zap.Error(err))
		}
	}()

	// Wait for shutdown signal
	<-sigChan
	logger.Info("shutdown signal received")

	// Graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer shutdownCancel()

	// Shutdown HTTP server
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("http server shutdown error", zap.Error(err))
	}

	// Shutdown proxy connections
	if err := proxy.Shutdown(shutdownCtx); err != nil {
		logger.Error("proxy shutdown error", zap.Error(err))
	}

	logger.Info("server stopped gracefully")
}
