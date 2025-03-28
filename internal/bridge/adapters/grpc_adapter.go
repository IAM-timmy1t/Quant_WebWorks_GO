// grpc_adapter.go - gRPC adapter for multi-language integration

package adapters

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/IAM-timmy1t/Quant_WebWork_GO/internal/core/config"
	"github.com/IAM-timmy1t/Quant_WebWork_GO/internal/core/metrics"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

// Common errors
var (
	ErrConnectionFailed   = errors.New("failed to establish gRPC connection")
	ErrServiceUnavailable = errors.New("gRPC service is unavailable")
	ErrDeadlineExceeded   = errors.New("gRPC request deadline exceeded")
	ErrInvalidArgument    = errors.New("invalid argument for gRPC request")
	ErrPermissionDenied   = errors.New("permission denied for gRPC request")
	ErrUnauthenticated    = errors.New("unauthenticated gRPC request")
	ErrCanceled           = errors.New("gRPC request was canceled")
)

// GRPCAdapterConfig contains configuration options for the gRPC adapter
type GRPCAdapterConfig struct {
	// Server configuration
	ServerAddress      string        // Address to bind the server to (e.g., "localhost:50051")
	MaxConcurrentCalls int           // Maximum number of concurrent calls
	Timeout            time.Duration // Default timeout for requests
	MaxRecvMsgSize     int           // Maximum message size for receiving in bytes
	MaxSendMsgSize     int           // Maximum message size for sending in bytes
	KeepaliveInterval  time.Duration // Interval for keepalive pings
	KeepaliveTimeout   time.Duration // Timeout for keepalive pings
	
	// Client configuration
	PoolSize            int           // Size of the connection pool
	DialTimeout         time.Duration // Timeout for establishing connections
	ClientKeepalive     time.Duration // Keepalive time for client connections
	ClientTimeout       time.Duration // Default client timeout for requests
	EnableRetry         bool          // Enable automatic retry for failed requests
	MaxRetries          int           // Maximum number of retries for a single request
	RetryBackoff        time.Duration // Backoff interval between retries
	LoadBalancingPolicy string        // Load balancing policy (e.g., "round_robin")
	
	// Security configuration
	EnableTLS            bool   // Enable TLS for connections
	TLSCertFile          string // Path to TLS certificate file
	TLSKeyFile           string // Path to TLS key file
	TLSCAFile            string // Path to CA certificate file for client verification
	ClientAuthType       string // Client authentication type (e.g., "RequireAndVerifyClientCert")
	EnableAuthentication bool   // Enable authentication for requests
	
	// Metrics and monitoring
	EnableMetrics      bool // Enable metrics collection
	EnableReflection   bool // Enable gRPC reflection service
	EnableHealthCheck  bool // Enable health checking
	EnableTracing      bool // Enable distributed tracing
}

// DefaultGRPCAdapterConfig returns the default configuration for the gRPC adapter
func DefaultGRPCAdapterConfig() *GRPCAdapterConfig {
	return &GRPCAdapterConfig{
		// Server configuration
		ServerAddress:      "localhost:50051",
		MaxConcurrentCalls: 100,
		Timeout:            time.Second * 30,
		MaxRecvMsgSize:     10 * 1024 * 1024, // 10MB
		MaxSendMsgSize:     10 * 1024 * 1024, // 10MB
		KeepaliveInterval:  time.Minute,
		KeepaliveTimeout:   time.Second * 20,
		
		// Client configuration
		PoolSize:            10,
		DialTimeout:         time.Second * 10,
		ClientKeepalive:     time.Minute,
		ClientTimeout:       time.Second * 30,
		EnableRetry:         true,
		MaxRetries:          3,
		RetryBackoff:        time.Second,
		LoadBalancingPolicy: "round_robin",
		
		// Security configuration
		EnableTLS:            false,
		EnableAuthentication: false,
		
		// Metrics and monitoring
		EnableMetrics:     true,
		EnableReflection:  true,
		EnableHealthCheck: true,
		EnableTracing:     true,
	}
}

// Logger interface for gRPC adapter logging
type Logger interface {
	Debug(msg string, fields map[string]interface{})
	Info(msg string, fields map[string]interface{})
	Warn(msg string, fields map[string]interface{})
	Error(msg string, fields map[string]interface{})
}

// GRPCAdapter provides a gRPC server and client for multi-language integration
type GRPCAdapter struct {
	config          *GRPCAdapterConfig
	server          *grpc.Server
	httpServer      *http.Server
	connections     map[string]*ConnectionPool
	connectionMutex sync.RWMutex
	logger          Logger
	metrics         *metrics.Collector
	configManager   *config.Manager
	serviceRegistry map[string]interface{}
	interceptors    []grpc.UnaryServerInterceptor
	streamInterceptors []grpc.StreamServerInterceptor
}

// ConnectionPool manages a pool of gRPC client connections
type ConnectionPool struct {
	target        string
	connections   []*grpc.ClientConn
	currentIndex  int
	mutex         sync.Mutex
	dialOptions   []grpc.DialOption
	size          int
	healthChecks  bool
}

// NewGRPCAdapter creates a new gRPC adapter
func NewGRPCAdapter(
	config *GRPCAdapterConfig,
	logger Logger,
	metrics *metrics.Collector,
	configManager *config.Manager,
) *GRPCAdapter {
	if config == nil {
		config = DefaultGRPCAdapterConfig()
	}
	
	// Create a new adapter instance
	adapter := &GRPCAdapter{
		config:          config,
		connections:     make(map[string]*ConnectionPool),
		logger:          logger,
		metrics:         metrics,
		configManager:   configManager,
		serviceRegistry: make(map[string]interface{}),
		interceptors:    make([]grpc.UnaryServerInterceptor, 0),
		streamInterceptors: make([]grpc.StreamServerInterceptor, 0),
	}
	
	// Add default interceptors
	adapter.AddInterceptor(adapter.loggingInterceptor)
	adapter.AddInterceptor(adapter.metricsInterceptor)
	adapter.AddInterceptor(adapter.recoveryInterceptor)
	adapter.AddInterceptor(adapter.timeoutInterceptor)
	
	// Initialize server
	adapter.initServer()
	
	return adapter
}

// initServer initializes the gRPC server with the configured options
func (a *GRPCAdapter) initServer() {
	// Build server options
	opts := []grpc.ServerOption{
		grpc.MaxConcurrentStreams(uint32(a.config.MaxConcurrentCalls)),
		grpc.MaxRecvMsgSize(a.config.MaxRecvMsgSize),
		grpc.MaxSendMsgSize(a.config.MaxSendMsgSize),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle:     a.config.KeepaliveInterval,
			MaxConnectionAge:      a.config.KeepaliveInterval * 2,
			MaxConnectionAgeGrace: a.config.KeepaliveTimeout,
			Time:                  a.config.KeepaliveInterval,
			Timeout:               a.config.KeepaliveTimeout,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             a.config.KeepaliveInterval / 2,
			PermitWithoutStream: true,
		}),
	}
	
	// Add TLS if enabled
	if a.config.EnableTLS {
		creds, err := a.getTLSCredentials()
		if err != nil {
			a.logger.Error("Failed to load TLS credentials", map[string]interface{}{
				"error": err.Error(),
			})
		} else {
			opts = append(opts, grpc.Creds(creds))
		}
	}
	
	// Add interceptors
	if len(a.interceptors) > 0 {
		opts = append(opts, grpc.ChainUnaryInterceptor(a.interceptors...))
	}
	
	// Add stream interceptors
	if len(a.streamInterceptors) > 0 {
		opts = append(opts, grpc.ChainStreamInterceptor(a.streamInterceptors...))
	}
	
	// Create the server
	a.server = grpc.NewServer(opts...)
	
	// Register reflection service if enabled
	if a.config.EnableReflection {
		reflection.Register(a.server)
	}
}

// getTLSCredentials loads TLS credentials for secure connections
func (a *GRPCAdapter) getTLSCredentials() (credentials.TransportCredentials, error) {
	// Check if certificate files are specified
	if a.config.TLSCertFile == "" || a.config.TLSKeyFile == "" {
		return nil, errors.New("TLS certificate or key file not specified")
	}
	
	// Load server certificate and key
	cert, err := tls.LoadX509KeyPair(a.config.TLSCertFile, a.config.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load server certificate and key: %w", err)
	}
	
	// Create TLS configuration
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.NoClientCert,
	}
	
	// Add client verification if CA file is specified
	if a.config.TLSCAFile != "" {
		// In a real implementation, you would load the CA certificate here
		// and set the appropriate client authentication type
		tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
	}
	
	return credentials.NewTLS(tlsConfig), nil
}

// RegisterService registers a gRPC service with the adapter
func (a *GRPCAdapter) RegisterService(name string, registerFunc func(s *grpc.Server)) {
	a.logger.Info("Registering gRPC service", map[string]interface{}{
		"service": name,
	})
	
	// Register the service
	registerFunc(a.server)
	
	// Add to service registry
	a.serviceRegistry[name] = true
	
	// Track metrics
	if a.metrics != nil {
		a.metrics.IncCounter("grpc_services_registered", map[string]string{
			"service": name,
		})
	}
}

// Start starts the gRPC server
func (a *GRPCAdapter) Start() error {
	addr := a.config.ServerAddress
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}
	
	a.logger.Info("Starting gRPC server", map[string]interface{}{
		"address": addr,
	})
	
	// Create a mux for the HTTP server
	mux := http.NewServeMux()
	
	// Add metrics endpoint
	mux.Handle("/metrics", promhttp.Handler())
	
	// Start server in a goroutine
	go func() {
		if err := a.server.Serve(listener); err != nil {
			a.logger.Error("gRPC server failed", map[string]interface{}{
				"error": err.Error(),
			})
		}
	}()
	
	// Start the HTTP server
	a.httpServer = &http.Server{
		Addr:    addr,
		Handler: mux,
	}
	
	// Log and record server start in metrics
	if a.metrics != nil {
		a.metrics.ServerUptime()
	}
	
	// Start the HTTP server in a goroutine
	go func() {
		if err := a.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			a.logger.Error("HTTP server error", map[string]interface{}{
				"error": err.Error(),
			})
		}
	}()
	
	return nil
}

// Stop gracefully stops the gRPC server
func (a *GRPCAdapter) Stop() {
	a.logger.Info("Stopping gRPC server", nil)
	
	// Close all client connections
	a.connectionMutex.Lock()
	for target, pool := range a.connections {
		a.logger.Debug("Closing connection pool", map[string]interface{}{
			"target": target,
		})
		pool.Close()
	}
	a.connections = make(map[string]*ConnectionPool)
	a.connectionMutex.Unlock()
	
	// Gracefully stop the server
	a.server.GracefulStop()
}

// AddInterceptor adds a unary interceptor to the server
func (a *GRPCAdapter) AddInterceptor(interceptor grpc.UnaryServerInterceptor) {
	a.interceptors = append(a.interceptors, interceptor)
}

// AddStreamInterceptor adds a stream interceptor to the server
func (a *GRPCAdapter) AddStreamInterceptor(interceptor grpc.StreamServerInterceptor) {
	a.streamInterceptors = append(a.streamInterceptors, interceptor)
}

// getConnection gets a connection from the pool or creates a new one
func (a *GRPCAdapter) getConnection(target string, options ...grpc.DialOption) (*grpc.ClientConn, error) {
	a.connectionMutex.RLock()
	pool, ok := a.connections[target]
	a.connectionMutex.RUnlock()
	
	if !ok {
		// Create a new connection pool
		a.connectionMutex.Lock()
		// Check again in case another goroutine created it
		pool, ok = a.connections[target]
		if !ok {
			// Create default dial options
			dialOpts := []grpc.DialOption{
				grpc.WithBlock(),
				grpc.WithDefaultCallOptions(
					grpc.MaxCallRecvMsgSize(a.config.MaxRecvMsgSize),
					grpc.MaxCallSendMsgSize(a.config.MaxSendMsgSize),
				),
				grpc.WithKeepaliveParams(keepalive.ClientParameters{
					Time:                a.config.ClientKeepalive,
					Timeout:             a.config.KeepaliveTimeout,
					PermitWithoutStream: true,
				}),
			}
			
			// Add custom options
			dialOpts = append(dialOpts, options...)
			
			// Add TLS if enabled
			if a.config.EnableTLS {
				creds, err := a.getTLSCredentials()
				if err != nil {
					a.connectionMutex.Unlock()
					return nil, fmt.Errorf("failed to load TLS credentials: %w", err)
				}
				dialOpts = append(dialOpts, grpc.WithTransportCredentials(creds))
			} else {
				dialOpts = append(dialOpts, grpc.WithInsecure())
			}
			
			// Create the connection pool
			pool = &ConnectionPool{
				target:      target,
				connections: make([]*grpc.ClientConn, 0, a.config.PoolSize),
				dialOptions: dialOpts,
				size:        a.config.PoolSize,
				healthChecks: a.config.EnableHealthCheck,
			}
			
			// Initialize the pool
			err := pool.Initialize()
			if err != nil {
				a.connectionMutex.Unlock()
				return nil, fmt.Errorf("failed to initialize connection pool: %w", err)
			}
			
			a.connections[target] = pool
		}
		a.connectionMutex.Unlock()
	}
	
	// Get a connection from the pool
	return pool.Get()
}

// Call makes a gRPC call to a service
func (a *GRPCAdapter) Call(
	ctx context.Context,
	target string,
	method string,
	request interface{},
	response interface{},
	options ...grpc.CallOption,
) error {
	// Create a context with timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, a.config.ClientTimeout)
	defer cancel()
	
	startTime := time.Now()
	
	// Get a connection
	conn, err := a.getConnection(target)
	if err != nil {
		a.logger.Error("Failed to get gRPC connection", map[string]interface{}{
			"target": target,
			"error":  err.Error(),
		})
		
		if a.metrics != nil {
			a.metrics.IncCounter("grpc_connection_errors", map[string]string{
				"target": target,
			})
		}
		
		return fmt.Errorf("%w: %v", ErrConnectionFailed, err)
	}
	
	// Make the call
	err = conn.Invoke(timeoutCtx, method, request, response, options...)
	
	// Record metrics
	if a.metrics != nil {
		a.metrics.IncCounter("grpc.requests_total", map[string]string{
			"method": method,
		})
		a.metrics.ObserveHistogram("grpc.request_duration_seconds", time.Since(startTime).Seconds(), map[string]string{
			"method": method,
		})
		
		if err != nil {
			a.metrics.IncCounter("grpc.errors_total", map[string]string{
				"method": method,
				"code":   grpcStatusCode(err),
			})
		}
	}
	
	// Map gRPC errors to meaningful errors
	if err != nil {
		return mapGRPCError(err)
	}
	
	return nil
}

// Stream creates a gRPC stream to a service
func (a *GRPCAdapter) Stream(
	ctx context.Context,
	target string,
	method string,
	options ...grpc.CallOption,
) (*grpc.ClientConn, error) {
	// Get a connection
	conn, err := a.getConnection(target)
	if err != nil {
		a.logger.Error("Failed to get gRPC connection for streaming", map[string]interface{}{
			"target": target,
			"error":  err.Error(),
		})
		
		if a.metrics != nil {
			a.metrics.IncCounter("grpc_stream_connection_errors", map[string]string{
				"target": target,
			})
		}
		
		return nil, fmt.Errorf("%w: %v", ErrConnectionFailed, err)
	}
	
	return conn, nil
}

// Initialize initializes the connection pool
func (p *ConnectionPool) Initialize() error {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	
	for i := 0; i < p.size; i++ {
		// Create connection with timeout
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
		conn, err := grpc.DialContext(ctx, p.target, p.dialOptions...)
		cancel()
		
		if err != nil {
			// Close any open connections
			for _, c := range p.connections {
				c.Close()
			}
			p.connections = nil
			return err
		}
		
		p.connections = append(p.connections, conn)
	}
	
	return nil
}

// Get gets a connection from the pool
func (p *ConnectionPool) Get() (*grpc.ClientConn, error) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	
	if len(p.connections) == 0 {
		return nil, errors.New("no connections available in pool")
	}
	
	// Simple round-robin
	conn := p.connections[p.currentIndex]
	p.currentIndex = (p.currentIndex + 1) % len(p.connections)
	
	return conn, nil
}

// Close closes all connections in the pool
func (p *ConnectionPool) Close() {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	
	for _, conn := range p.connections {
		conn.Close()
	}
	
	p.connections = nil
}

// loggingInterceptor logs incoming requests
func (a *GRPCAdapter) loggingInterceptor(
	ctx context.Context,
	req interface{},
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (interface{}, error) {
	startTime := time.Now()
	
	// Extract metadata from context
	// In a real implementation, you would extract request ID, user info, etc.
	
	// Log the request
	a.logger.Debug("gRPC request", map[string]interface{}{
		"method": info.FullMethod,
		// Add metadata here
	})
	
	// Call the handler
	resp, err := handler(ctx, req)
	
	// Log the response
	if err != nil {
		a.logger.Warn("gRPC request failed", map[string]interface{}{
			"method":   info.FullMethod,
			"error":    err.Error(),
			"duration": time.Since(startTime).String(),
		})
	} else {
		a.logger.Debug("gRPC request completed", map[string]interface{}{
			"method":   info.FullMethod,
			"duration": time.Since(startTime).String(),
		})
	}
	
	// Record metrics before each request if enabled
	if a.metrics != nil {
		a.metrics.IncCounter("grpc.requests_total", map[string]string{
			"method": info.FullMethod,
		})
	}
	
	return resp, err
}

// metricsInterceptor collects metrics for requests
func (a *GRPCAdapter) metricsInterceptor(
	ctx context.Context,
	req interface{},
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (interface{}, error) {
	startTime := time.Now()
	
	// Call the handler
	resp, err := handler(ctx, req)
	
	// Record metrics
	if a.metrics != nil {
		a.metrics.ObserveHistogram("grpc.request_duration_seconds", time.Since(startTime).Seconds(), map[string]string{
			"method": info.FullMethod,
		})
		
		if err != nil {
			a.metrics.IncCounter("grpc.errors_total", map[string]string{
				"method": info.FullMethod,
				"code":   grpcStatusCode(err),
			})
		}
	}
	
	return resp, err
}

// recoveryInterceptor recovers from panics
func (a *GRPCAdapter) recoveryInterceptor(
	ctx context.Context,
	req interface{},
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (resp interface{}, err error) {
	defer func() {
		if r := recover(); r != nil {
			// Log the panic
			a.logger.Error("gRPC handler panic", map[string]interface{}{
				"method": info.FullMethod,
				"panic":  fmt.Sprintf("%v", r),
				// In a real implementation, you would include stack trace
			})
			
			// Return a gRPC error
			err = status.Errorf(codes.Internal, "internal error")
			
			// Record metrics if enabled
			if a.metrics != nil {
				a.metrics.IncCounter("grpc.panics_total", map[string]string{
					"method": info.FullMethod,
				})
			}
		}
	}()
	
	return handler(ctx, req)
}

// timeoutInterceptor adds timeout to requests
func (a *GRPCAdapter) timeoutInterceptor(
	ctx context.Context,
	req interface{},
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (interface{}, error) {
	// Create a context with timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, a.config.Timeout)
	defer cancel()
	
	return handler(timeoutCtx, req)
}

// grpcStatusCode extracts the status code from a gRPC error
func grpcStatusCode(err error) string {
	if err == nil {
		return "ok"
	}
	
	st, ok := status.FromError(err)
	if !ok {
		return "unknown"
	}
	
	return st.Code().String()
}

// mapGRPCError maps gRPC errors to more meaningful errors
func mapGRPCError(err error) error {
	if err == nil {
		return nil
	}
	
	st, ok := status.FromError(err)
	if !ok {
		return errors.New("unknown error")
	}
	
	switch st.Code() {
	case codes.InvalidArgument:
		return NewBridgeError("INVALID_ARGUMENT", st.Message(), map[string]interface{}{
			"code": int(codes.InvalidArgument),
			"details": extractErrorDetails(st),
		})
	case codes.NotFound:
		return NewBridgeError("NOT_FOUND", st.Message(), map[string]interface{}{
			"code": int(codes.NotFound),
			"details": extractErrorDetails(st),
		})
	case codes.AlreadyExists:
		return NewBridgeError("ALREADY_EXISTS", st.Message(), map[string]interface{}{
			"code": int(codes.AlreadyExists),
			"details": extractErrorDetails(st),
		})
	case codes.PermissionDenied:
		return NewBridgeError("PERMISSION_DENIED", st.Message(), map[string]interface{}{
			"code": int(codes.PermissionDenied),
			"details": extractErrorDetails(st),
		})
	case codes.ResourceExhausted:
		return NewBridgeError("RESOURCE_EXHAUSTED", st.Message(), map[string]interface{}{
			"code": int(codes.ResourceExhausted),
			"details": extractErrorDetails(st),
		})
	case codes.FailedPrecondition:
		return NewBridgeError("FAILED_PRECONDITION", st.Message(), map[string]interface{}{
			"code": int(codes.FailedPrecondition),
			"details": extractErrorDetails(st),
		})
	case codes.Aborted:
		return NewBridgeError("ABORTED", st.Message(), map[string]interface{}{
			"code": int(codes.Aborted),
			"details": extractErrorDetails(st),
		})
	case codes.OutOfRange:
		return NewBridgeError("OUT_OF_RANGE", st.Message(), map[string]interface{}{
			"code": int(codes.OutOfRange),
			"details": extractErrorDetails(st),
		})
	case codes.Unimplemented:
		return NewBridgeError("UNIMPLEMENTED", st.Message(), map[string]interface{}{
			"code": int(codes.Unimplemented),
			"details": extractErrorDetails(st),
		})
	case codes.Internal:
		return NewBridgeError("INTERNAL", st.Message(), map[string]interface{}{
			"code": int(codes.Internal),
			"details": extractErrorDetails(st),
		})
	case codes.Unavailable:
		return NewBridgeError("UNAVAILABLE", st.Message(), map[string]interface{}{
			"code": int(codes.Unavailable),
			"details": extractErrorDetails(st),
		})
	case codes.DataLoss:
		return NewBridgeError("DATA_LOSS", st.Message(), map[string]interface{}{
			"code": int(codes.DataLoss),
			"details": extractErrorDetails(st),
		})
	case codes.Unauthenticated:
		return NewBridgeError("UNAUTHENTICATED", st.Message(), map[string]interface{}{
			"code": int(codes.Unauthenticated),
			"details": extractErrorDetails(st),
		})
	case codes.DeadlineExceeded:
		return NewBridgeError("DEADLINE_EXCEEDED", st.Message(), map[string]interface{}{
			"code": int(codes.DeadlineExceeded),
			"details": extractErrorDetails(st),
		})
	case codes.Canceled:
		return NewBridgeError("CANCELED", st.Message(), map[string]interface{}{
			"code": int(codes.Canceled),
			"details": extractErrorDetails(st),
		})
	default:
		return NewBridgeError("UNKNOWN", st.Message(), map[string]interface{}{
			"code": int(st.Code()),
			"details": extractErrorDetails(st),
		})
	}
}

// extractErrorDetails extracts details from a gRPC status
func extractErrorDetails(st *status.Status) []map[string]interface{} {
	details := []map[string]interface{}{}
	
	for _, detail := range st.Details() {
		switch d := detail.(type) {
		case *errdetails.BadRequest:
			fieldViolations := []map[string]string{}
			for _, violation := range d.GetFieldViolations() {
				fieldViolations = append(fieldViolations, map[string]string{
					"field":       violation.GetField(),
					"description": violation.GetDescription(),
				})
			}
			details = append(details, map[string]interface{}{
				"type":            "BadRequest",
				"fieldViolations": fieldViolations,
			})
		case *errdetails.RequestInfo:
			details = append(details, map[string]interface{}{
				"type":        "RequestInfo",
				"requestId":   d.GetRequestId(),
				"servingData": d.GetServingData(),
			})
		case *errdetails.ErrorInfo:
			details = append(details, map[string]interface{}{
				"type":     "ErrorInfo",
				"reason":   d.GetReason(),
				"domain":   d.GetDomain(),
				"metadata": d.GetMetadata(),
			})
		case *errdetails.PreconditionFailure:
			violations := []map[string]string{}
			for _, violation := range d.GetViolations() {
				violations = append(violations, map[string]string{
					"type":        violation.GetType(),
					"subject":     violation.GetSubject(),
					"description": violation.GetDescription(),
				})
			}
			details = append(details, map[string]interface{}{
				"type":       "PreconditionFailure",
				"violations": violations,
			})
		case *errdetails.QuotaFailure:
			violations := []map[string]string{}
			for _, violation := range d.GetViolations() {
				violations = append(violations, map[string]string{
					"subject":     violation.GetSubject(),
					"description": violation.GetDescription(),
				})
			}
			details = append(details, map[string]interface{}{
				"type":       "QuotaFailure",
				"violations": violations,
			})
		default:
			details = append(details, map[string]interface{}{
				"type":  "Unknown",
				"value": fmt.Sprintf("%v", d),
			})
		}
	}
	
	return details
}

// BridgeError represents an error in the bridge module
type BridgeError struct {
	Code    string                 `json:"code"`
	Message string                 `json:"message"`
	Details map[string]interface{} `json:"details,omitempty"`
}

// NewBridgeError creates a new bridge error
func NewBridgeError(code string, message string, details map[string]interface{}) *BridgeError {
	return &BridgeError{
		Code:    code,
		Message: message,
		Details: details,
	}
}

// Error returns the error message
func (e *BridgeError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}
