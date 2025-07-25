/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package servenv

import (
	"context"
	"crypto/tls"
	"math"
	"net"
	"strconv"
	"time"

	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/spf13/pflag"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/orca"
	"google.golang.org/grpc/reflection"

	"vitess.io/vitess/go/trace"
	"vitess.io/vitess/go/vt/grpccommon"
	"vitess.io/vitess/go/vt/grpcoptionaltls"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/utils"
	"vitess.io/vitess/go/vt/vttls"
)

// This file handles gRPC server, on its own port.
// Clients register servers, based on service map:
//
// servenv.RegisterGRPCFlags()
//
//	servenv.OnRun(func() {
//	  if servenv.GRPCCheckServiceMap("XXX") {
//	    pb.RegisterXXX(servenv.GRPCServer, XXX)
//	  }
//	}
//
// Note servenv.GRPCServer can only be used in servenv.OnRun,
// and not before, as it is initialized right before calling OnRun.
var (
	// gRPCAuth specifies which auth plugin to use. Currently only "static" and
	// "mtls" are supported.
	//
	// To expose this flag, call RegisterGRPCAuthServerFlags before ParseFlags.
	gRPCAuth string

	// GRPCServer is the global server to serve gRPC.
	GRPCServer *grpc.Server

	// GRPC server metrics recorder
	GRPCServerMetricsRecorder orca.ServerMetricsRecorder

	authPlugin Authenticator
)

// Misc. server variables.
var (
	// gRPCPort is the port to listen on for gRPC. If zero, don't listen.
	gRPCPort int

	// gRPCBindAddress is the address to bind to for gRPC. If empty, bind to all addresses.
	gRPCBindAddress string

	// gRPCMaxConnectionAge is the maximum age of a client connection, before GoAway is sent.
	// This is useful for L4 loadbalancing to ensure rebalancing after scaling.
	gRPCMaxConnectionAge = time.Duration(math.MaxInt64)

	// gRPCMaxConnectionAgeGrace is an additional grace period after GRPCMaxConnectionAge, after which
	// connections are forcibly closed.
	gRPCMaxConnectionAgeGrace = time.Duration(math.MaxInt64)

	// gRPCInitialConnWindowSize ServerOption that sets window size for a connection.
	// The lower bound for window size is 64K and any value smaller than that will be ignored.
	gRPCInitialConnWindowSize int

	// gRPCInitialWindowSize ServerOption that sets window size for stream.
	// The lower bound for window size is 64K and any value smaller than that will be ignored.
	gRPCInitialWindowSize int

	// gRPCKeepAliveEnforcementPolicyMinTime sets the keepalive enforcement policy on the server.
	// This is the minimum amount of time a client should wait before sending a keepalive ping.
	gRPCKeepAliveEnforcementPolicyMinTime = 10 * time.Second

	// gRPCKeepAliveEnforcementPolicyPermitWithoutStream, if true, instructs the server to allow keepalive pings
	// even when there are no active streams (RPCs). If false, and client sends ping when
	// there are no active streams, server will send GOAWAY and close the connection.
	gRPCKeepAliveEnforcementPolicyPermitWithoutStream bool

	// Enable ORCA metrics to be sent from the server to the client to be used for load balancing.
	gRPCEnableOrcaMetrics bool

	gRPCKeepaliveTime    = 10 * time.Second
	gRPCKeepaliveTimeout = 10 * time.Second
)

// Injectable behavior for testing.
var (
	orcaRegisterFunc = orca.Register
)

// TLS variables.
var (
	// gRPCCert is the cert to use if TLS is enabled.
	gRPCCert string
	// gRPCKey is the key to use if TLS is enabled.
	gRPCKey string
	// gRPCCA is the CA to use if TLS is enabled.
	gRPCCA string
	// gRPCCRL is the CRL (Certificate Revocation List) to use if TLS is
	// enabled.
	gRPCCRL string
	// gRPCEnableOptionalTLS enables an optional TLS mode when a server accepts
	// both TLS and plain-text connections on the same port.
	gRPCEnableOptionalTLS bool
	// gRPCServerCA if specified will combine server cert and server CA.
	gRPCServerCA string
)

// RegisterGRPCServerFlags registers flags required to run a gRPC server via Run
// or RunDefault.
//
// `go/cmd/*` entrypoints should call this function before
// ParseFlags(WithArgs)? if they wish to run a gRPC server.
func RegisterGRPCServerFlags() {
	OnParse(func(fs *pflag.FlagSet) {
		utils.SetFlagIntVar(fs, &gRPCPort, "grpc-port", gRPCPort, "Port to listen on for gRPC calls. If zero, do not listen.")
		utils.SetFlagStringVar(fs, &gRPCBindAddress, "grpc-bind-address", gRPCBindAddress, "Bind address for gRPC calls. If empty, listen on all addresses.")
		utils.SetFlagDurationVar(fs, &gRPCMaxConnectionAge, "grpc-max-connection-age", gRPCMaxConnectionAge, "Maximum age of a client connection before GoAway is sent.")
		utils.SetFlagDurationVar(fs, &gRPCMaxConnectionAgeGrace, "grpc-max-connection-age-grace", gRPCMaxConnectionAgeGrace, "Additional grace period after grpc-max-connection-age, after which connections are forcibly closed.")
		utils.SetFlagIntVar(fs, &gRPCInitialConnWindowSize, "grpc-server-initial-conn-window-size", gRPCInitialConnWindowSize, "gRPC server initial connection window size")
		utils.SetFlagIntVar(fs, &gRPCInitialWindowSize, "grpc-server-initial-window-size", gRPCInitialWindowSize, "gRPC server initial window size")
		utils.SetFlagDurationVar(fs, &gRPCKeepAliveEnforcementPolicyMinTime, "grpc-server-keepalive-enforcement-policy-min-time", gRPCKeepAliveEnforcementPolicyMinTime, "gRPC server minimum keepalive time")
		utils.SetFlagBoolVar(fs, &gRPCKeepAliveEnforcementPolicyPermitWithoutStream, "grpc-server-keepalive-enforcement-policy-permit-without-stream", gRPCKeepAliveEnforcementPolicyPermitWithoutStream, "gRPC server permit client keepalive pings even when there are no active streams (RPCs)")
		utils.SetFlagBoolVar(fs, &gRPCEnableOrcaMetrics, "grpc-enable-orca-metrics", gRPCEnableOrcaMetrics, "gRPC server option to enable sending ORCA metrics to clients for load balancing")

		utils.SetFlagStringVar(fs, &gRPCCert, "grpc-cert", gRPCCert, "server certificate to use for gRPC connections, requires grpc-key, enables TLS")
		utils.SetFlagStringVar(fs, &gRPCKey, "grpc-key", gRPCKey, "server private key to use for gRPC connections, requires grpc-cert, enables TLS")
		utils.SetFlagStringVar(fs, &gRPCCA, "grpc-ca", gRPCCA, "server CA to use for gRPC connections, requires TLS, and enforces client certificate check")
		utils.SetFlagStringVar(fs, &gRPCCRL, "grpc-crl", gRPCCRL, "path to a certificate revocation list in PEM format, client certificates will be further verified against this file during TLS handshake")
		utils.SetFlagBoolVar(fs, &gRPCEnableOptionalTLS, "grpc-enable-optional-tls", gRPCEnableOptionalTLS, "enable optional TLS mode when a server accepts both TLS and plain-text connections on the same port")
		utils.SetFlagStringVar(fs, &gRPCServerCA, "grpc-server-ca", gRPCServerCA, "path to server CA in PEM format, which will be combine with server cert, return full certificate chain to clients")
		utils.SetFlagDurationVar(fs, &gRPCKeepaliveTime, "grpc-server-keepalive-time", gRPCKeepaliveTime, "After a duration of this time, if the server doesn't see any activity, it pings the client to see if the transport is still alive.")
		utils.SetFlagDurationVar(fs, &gRPCKeepaliveTimeout, "grpc-server-keepalive-timeout", gRPCKeepaliveTimeout, "After having pinged for keepalive check, the server waits for a duration of Timeout and if no activity is seen even after that the connection is closed.")
	})
}

// GRPCCert returns the value of the `--grpc-cert` flag.
func GRPCCert() string {
	return gRPCCert
}

// GRPCCertificateAuthority returns the value of the `--grpc-ca` flag.
func GRPCCertificateAuthority() string {
	return gRPCCA
}

// GRPCKey returns the value of the `--grpc-key` flag.
func GRPCKey() string {
	return gRPCKey
}

// GRPCPort returns the value of the `--grpc-port` flag.
func GRPCPort() int {
	return gRPCPort
}

// GRPCPort returns the value of the `--grpc-bind-address` flag.
func GRPCBindAddress() string {
	return gRPCBindAddress
}

// isGRPCEnabled returns true if gRPC server is set
func isGRPCEnabled() bool {
	if gRPCPort != 0 {
		return true
	}

	if socketFile != "" {
		return true
	}

	return false
}

// createGRPCServer create the gRPC server we will be using.
// It has to be called after flags are parsed, but before
// services register themselves.
func createGRPCServer() {
	// skip if not registered
	if !isGRPCEnabled() {
		log.Infof("Skipping gRPC server creation")
		return
	}

	var opts []grpc.ServerOption
	if gRPCCert != "" && gRPCKey != "" {
		config, err := vttls.ServerConfig(gRPCCert, gRPCKey, gRPCCA, gRPCCRL, gRPCServerCA, tls.VersionTLS12)
		if err != nil {
			log.Exitf("Failed to log gRPC cert/key/ca: %v", err)
		}

		// create the creds server options
		creds := credentials.NewTLS(config)
		if gRPCEnableOptionalTLS {
			log.Warning("Optional TLS is active. Plain-text connections will be accepted")
			creds = grpcoptionaltls.New(creds)
		}
		opts = []grpc.ServerOption{grpc.Creds(creds)}
	}
	// Override the default max message size for both send and receive
	// (which is 4 MiB in gRPC 1.0.0).
	// Large messages can occur when users try to insert or fetch very big
	// rows. If they hit the limit, they'll see the following error:
	// grpc: received message length XXXXXXX exceeding the max size 4194304
	// Note: For gRPC 1.0.0 it's sufficient to set the limit on the server only
	// because it's not enforced on the client side.
	msgSize := grpccommon.MaxMessageSize()
	log.Infof("Setting grpc max message size to %d", msgSize)
	opts = append(opts, grpc.MaxRecvMsgSize(msgSize))
	opts = append(opts, grpc.MaxSendMsgSize(msgSize))

	if gRPCEnableOrcaMetrics {
		GRPCServerMetricsRecorder = orca.NewServerMetricsRecorder()
		opts = append(opts, orca.CallMetricsServerOption(GRPCServerMetricsRecorder))
	}

	if gRPCInitialConnWindowSize != 0 {
		log.Infof("Setting grpc server initial conn window size to %d", int32(gRPCInitialConnWindowSize))
		opts = append(opts, grpc.InitialConnWindowSize(int32(gRPCInitialConnWindowSize)))
	}

	if gRPCInitialWindowSize != 0 {
		log.Infof("Setting grpc server initial window size to %d", int32(gRPCInitialWindowSize))
		opts = append(opts, grpc.InitialWindowSize(int32(gRPCInitialWindowSize)))
	}

	ep := keepalive.EnforcementPolicy{
		MinTime:             gRPCKeepAliveEnforcementPolicyMinTime,
		PermitWithoutStream: gRPCKeepAliveEnforcementPolicyPermitWithoutStream,
	}
	opts = append(opts, grpc.KeepaliveEnforcementPolicy(ep))

	ka := keepalive.ServerParameters{
		MaxConnectionAge:      gRPCMaxConnectionAge,
		MaxConnectionAgeGrace: gRPCMaxConnectionAgeGrace,
		Time:                  gRPCKeepaliveTime,
		Timeout:               gRPCKeepaliveTimeout,
	}
	opts = append(opts, grpc.KeepaliveParams(ka))

	opts = append(opts, interceptors()...)

	GRPCServer = grpc.NewServer(opts...)
}

// We can only set a ServerInterceptor once, so we chain multiple interceptors into one
func interceptors() []grpc.ServerOption {
	interceptors := &serverInterceptorBuilder{}

	if gRPCAuth != "" {
		log.Infof("enabling auth plugin %v", gRPCAuth)
		pluginInitializer := GetAuthenticator(gRPCAuth)
		authPluginImpl, err := pluginInitializer()
		if err != nil {
			log.Fatalf("Failed to load auth plugin: %v", err)
		}
		authPlugin = authPluginImpl
		interceptors.Add(authenticatingStreamInterceptor, authenticatingUnaryInterceptor)
	}

	if grpccommon.EnableGRPCPrometheus() {
		interceptors.Add(grpc_prometheus.StreamServerInterceptor, grpc_prometheus.UnaryServerInterceptor)
	}

	trace.AddGrpcServerOptions(interceptors.Add)

	return interceptors.Build()
}

func serveGRPC() {
	if grpccommon.EnableGRPCPrometheus() {
		grpc_prometheus.Register(GRPCServer)
		grpc_prometheus.EnableHandlingTimeHistogram()
	}
	// skip if not registered
	if gRPCPort == 0 {
		return
	}

	if gRPCEnableOrcaMetrics {
		registerOrca()
	}

	// register reflection to support list calls :)
	reflection.Register(GRPCServer)

	// register health service to support health checks
	healthServer := health.NewServer()
	healthpb.RegisterHealthServer(GRPCServer, healthServer)

	for service := range GRPCServer.GetServiceInfo() {
		healthServer.SetServingStatus(service, healthpb.HealthCheckResponse_SERVING)
	}

	// listen on the port
	log.Infof("Listening for gRPC calls on port %v", gRPCPort)
	listener, err := net.Listen("tcp", net.JoinHostPort(gRPCBindAddress, strconv.Itoa(gRPCPort)))
	if err != nil {
		log.Exitf("Cannot listen on port %v for gRPC: %v", gRPCPort, err)
	}

	// and serve on it
	// NOTE: Before we call Serve(), all services must have registered themselves
	//       with "GRPCServer". This is the case because go/vt/servenv/run.go
	//       runs all OnRun() hooks after createGRPCServer() and before
	//       serveGRPC(). If this was not the case, the binary would crash with
	//       the error "grpc: Server.RegisterService after Server.Serve".
	go func() {
		err := GRPCServer.Serve(listener)
		if err != nil {
			log.Exitf("Failed to start grpc server: %v", err)
		}
	}()

	OnTermSync(func() {
		log.Info("Initiated graceful stop of gRPC server")
		GRPCServer.GracefulStop()
		log.Info("gRPC server stopped")
	})
}

func registerOrca() {
	if err := orcaRegisterFunc(GRPCServer, orca.ServiceOptions{
		// The minimum interval of orca is 30 seconds, unless we enable a testing flag.
		MinReportingInterval:  30 * time.Second,
		ServerMetricsProvider: GRPCServerMetricsRecorder,
	}); err != nil {
		log.Exitf("Failed to register ORCA service: %v", err)
	}

	// Initialize the server metrics values.
	GRPCServerMetricsRecorder.SetCPUUtilization(getCpuUsage())
	GRPCServerMetricsRecorder.SetMemoryUtilization(getMemoryUsage())

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			GRPCServerMetricsRecorder.SetCPUUtilization(getCpuUsage())
			GRPCServerMetricsRecorder.SetMemoryUtilization(getMemoryUsage())
		}
	}()
}

// GRPCCheckServiceMap returns if we should register a gRPC service
// (and also logs how to enable / disable it)
func GRPCCheckServiceMap(name string) bool {
	// Silently fail individual services if gRPC is not enabled in
	// the first place (either on a grpc port or on the socket file)
	if !isGRPCEnabled() {
		return false
	}

	// then check ServiceMap
	return checkServiceMap("grpc", name)
}

func authenticatingStreamInterceptor(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	newCtx, err := authPlugin.Authenticate(stream.Context(), info.FullMethod)

	if err != nil {
		return err
	}

	wrapped := WrapServerStream(stream)
	wrapped.WrappedContext = newCtx
	return handler(srv, wrapped)
}

func authenticatingUnaryInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	newCtx, err := authPlugin.Authenticate(ctx, info.FullMethod)
	if err != nil {
		return nil, err
	}

	return handler(newCtx, req)
}

// WrappedServerStream is based on the service stream wrapper from: https://github.com/grpc-ecosystem/go-grpc-middleware
type WrappedServerStream struct {
	grpc.ServerStream
	WrappedContext context.Context
}

// Context returns the wrapper's WrappedContext, overwriting the nested grpc.ServerStream.Context()
func (w *WrappedServerStream) Context() context.Context {
	return w.WrappedContext
}

// WrapServerStream returns a ServerStream that has the ability to overwrite context.
func WrapServerStream(stream grpc.ServerStream) *WrappedServerStream {
	if existing, ok := stream.(*WrappedServerStream); ok {
		return existing
	}
	return &WrappedServerStream{ServerStream: stream, WrappedContext: stream.Context()}
}

// serverInterceptorBuilder chains together multiple ServerInterceptors
type serverInterceptorBuilder struct {
	streamInterceptors []grpc.StreamServerInterceptor
	unaryInterceptors  []grpc.UnaryServerInterceptor
}

// Add adds interceptors to the builder
func (collector *serverInterceptorBuilder) Add(s grpc.StreamServerInterceptor, u grpc.UnaryServerInterceptor) {
	collector.streamInterceptors = append(collector.streamInterceptors, s)
	collector.unaryInterceptors = append(collector.unaryInterceptors, u)
}

// AddUnary adds a single unary interceptor to the builder
func (collector *serverInterceptorBuilder) AddUnary(u grpc.UnaryServerInterceptor) {
	collector.unaryInterceptors = append(collector.unaryInterceptors, u)
}

// Build returns DialOptions to add to the grpc.Dial call
func (collector *serverInterceptorBuilder) Build() []grpc.ServerOption {
	log.Infof("Building interceptors with %d unary interceptors and %d stream interceptors", len(collector.unaryInterceptors), len(collector.streamInterceptors))
	switch len(collector.unaryInterceptors) + len(collector.streamInterceptors) {
	case 0:
		return []grpc.ServerOption{}
	default:
		return []grpc.ServerOption{
			grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(collector.unaryInterceptors...)),
			grpc.StreamInterceptor(grpc_middleware.ChainStreamServer(collector.streamInterceptors...)),
		}
	}
}
