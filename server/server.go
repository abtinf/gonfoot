/*
Package server provides a production ready GRPC/HTTP server.
*/
package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"gonfoot/config"
	"gonfoot/db"
	"gonfoot/db/sql/migrations"
	"gonfoot/static"

	pb "gonfoot/proto"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
)

type serverConfig struct {
	*config.Config
	httpAddr        string
	dsn             string
	shutdownTimeout time.Duration
	monitorInterval time.Duration
}

type server struct {
	pb.UnimplementedAPIServer

	ctx    context.Context
	config serverConfig
	log    *slog.Logger
	mux    *http.ServeMux
	db     *db.DB

	live                atomic.Bool
	ready               atomic.Bool
	shutdownRequested   atomic.Bool
	httpServerAvailable atomic.Bool
	databaseAvailable   atomic.Bool

	httpServer *http.Server
	httpClosed chan bool
}

/*
New creates a new server with the provided configuration.
*/
func New(ctx context.Context, log *slog.Logger, config *config.Config) (*server, error) {
	s := &server{
		ctx:    ctx,
		config: serverConfig{Config: config},
		log:    log,

		httpClosed: make(chan bool),
	}
	s.config.monitorInterval = time.Duration(config.MonitorInterval) * time.Second
	s.config.shutdownTimeout = time.Duration(config.HttpShutdownGracePeriod) * time.Second
	s.config.httpAddr = net.JoinHostPort(config.HttpHost, strconv.Itoa(config.HttpPort))
	s.config.dsn = fmt.Sprintf("postgres://%s:%s@%s:%d/%s?search_path=%s", config.PostgresUsername, config.PostgresPassword, config.PostgresHost, config.PostgresPort, config.PostgresDatabase, config.PostgresSchema)
	s.db = db.New(s.ctx, s.config.dsn, log)

	s.live.Store(false)
	s.ready.Store(false)
	s.shutdownRequested.Store(false)
	s.httpServerAvailable.Store(false)
	s.databaseAvailable.Store(false)

	grpcOptions := []grpc.ServerOption{grpc.Creds(insecure.NewCredentials())}
	grpcServer := grpc.NewServer(grpcOptions...)
	pb.RegisterAPIServer(grpcServer, s)
	grpcMux := runtime.NewServeMux(runtime.WithOutgoingHeaderMatcher(func(s string) (string, bool) {
		return s, true
	}), runtime.WithMarshalerOption("*", &httpBodyMarshaler{
		delimeter: []byte(""),
		HTTPBodyMarshaler: runtime.HTTPBodyMarshaler{
			Marshaler: &runtime.JSONPb{
				MarshalOptions:   protojson.MarshalOptions{EmitUnpopulated: true},
				UnmarshalOptions: protojson.UnmarshalOptions{DiscardUnknown: true},
			},
		},
	}))

	dialOptions := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	if err := pb.RegisterAPIHandlerFromEndpoint(s.ctx, grpcMux, s.config.httpAddr, dialOptions); err != nil {
		return nil, fmt.Errorf("failed to register gateway: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", requireBasicAuth(s, http.NotFoundHandler()))
	mux.Handle("/favicon.ico", http.FileServer(http.FS(static.Http)))
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(static.Http))))
	mux.Handle("/metrics", promhttp.Handler())
	mux.Handle("/api/", http.StripPrefix("/api", onlyWhenReady(s, logger(s, grpcMux))))

	mux.Handle("/examplereverseproxy/", logger(s, mustReverseProxy(s, s.config.ExampleReverseProxyURL)))
	s.mux = mux

	s.httpServer = &http.Server{
		Addr:    s.config.httpAddr,
		Handler: h2c.NewHandler(upgradeHandler(s.mux, grpcServer), &http2.Server{}),
	}

	return s, nil
}

func (s *server) ListenAndServe() error {
	go s.listenAndServe()
	go s.liveMonitor()
	go s.readyMonitor()
	go s.dbMonitor()

	if err := s.db.Connect(); err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	if err := migrations.Migrate(s.ctx, s.log, s.db); err != nil {
		s.log.Error("db migration failed", "err", err)
		return nil
	}

	for {
		select {
		case <-s.httpClosed:
			s.log.Info("http server closed")
			s.httpServerAvailable.CompareAndSwap(true, false)
			return nil
		case <-s.ctx.Done():
			s.log.Info("shutdown signal received", "signal", s.ctx.Err())
			s.httpServerAvailable.CompareAndSwap(true, false)
			s.shutdownRequested.CompareAndSwap(false, true)
			ctx, cancel := context.WithTimeout(context.Background(), s.config.shutdownTimeout)
			defer cancel()
			if err := s.httpServer.Shutdown(ctx); err != nil {
				return fmt.Errorf("error during http server shutdown: %w", err)
			} else {
				s.log.Info("http server shutdown gracefully")
			}
			return nil
		}
	}
}

func (s *server) listenAndServe() {
	s.httpServerAvailable.Store(true)
	lis, err := net.Listen("tcp", s.config.httpAddr)
	if err != nil {
		s.log.Error("Failed to listen", "err", err, "addr", s.config.httpAddr)
		s.httpClosed <- true
		return
	}
	if err := s.httpServer.Serve(lis); err != nil && err != http.ErrServerClosed {
		s.log.Error("Unexpected http server error", "err", err, "addr", s.config.httpAddr)
		s.httpServerAvailable.CompareAndSwap(true, false)
		s.httpClosed <- true
	}
}
