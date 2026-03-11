package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection"

	"EXBanka/internal/config"
	"EXBanka/internal/database"
	"EXBanka/internal/middleware"
	infrasvc "EXBanka/internal/service"
	"EXBanka/backend/internal/handler"
	"EXBanka/backend/internal/swagger"

	authv1         "EXBanka/gen/proto/auth/v1"
	employeev1     "EXBanka/gen/proto/employee/v1"
	notificationv1 "EXBanka/gen/proto/notification/v1"
)

func main() {
	cfg := config.Load()

	db, err := database.Connect(cfg)
	if err != nil {
		log.Fatalf("DB connection failed: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		log.Fatalf("DB migration failed: %v", err)
	}
	if err := database.SeedPermissions(db); err != nil {
		log.Fatalf("Permission seeding failed: %v", err)
	}

	notifSvc := infrasvc.NewNotificationService(cfg)

	authH     := handler.NewAuthHandler(cfg, db, notifSvc)
	employeeH := handler.NewEmployeeHandler(cfg, db, notifSvc)
	notifH    := handler.NewNotificationHandler(cfg, notifSvc)

	grpcServer := grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			middleware.LoggingInterceptor(),
			middleware.AuthInterceptor(cfg),
		),
	)

	authv1.RegisterAuthServiceServer(grpcServer, authH)
	employeev1.RegisterEmployeeServiceServer(grpcServer, employeeH)
	notificationv1.RegisterNotificationServiceServer(grpcServer, notifH)
	reflection.Register(grpcServer)

	grpcLis, err := net.Listen("tcp", ":"+cfg.GRPCPort)
	if err != nil {
		log.Fatalf("gRPC listen failed: %v", err)
	}

	go func() {
		log.Printf("gRPC server listening on :%s", cfg.GRPCPort)
		if err := grpcServer.Serve(grpcLis); err != nil {
			log.Fatalf("gRPC server error: %v", err)
		}
	}()

	ctx := context.Background()
	gwMux := runtime.NewServeMux()
	dialOpts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	grpcEndpoint := "localhost:" + cfg.GRPCPort

	if err := authv1.RegisterAuthServiceHandlerFromEndpoint(ctx, gwMux, grpcEndpoint, dialOpts); err != nil {
		log.Fatalf("Failed to register auth HTTP gateway: %v", err)
	}
	if err := employeev1.RegisterEmployeeServiceHandlerFromEndpoint(ctx, gwMux, grpcEndpoint, dialOpts); err != nil {
		log.Fatalf("Failed to register employee HTTP gateway: %v", err)
	}
	// NotificationService is intentionally NOT exposed via HTTP gateway

	httpMux := http.NewServeMux()
	httpMux.HandleFunc("/swagger.json", swagger.HandlerJSON)
	httpMux.HandleFunc("/swagger-ui", swagger.HandlerUI)
	httpMux.HandleFunc("/health", healthCheck)
	httpMux.Handle("/", middleware.CORS(gwMux))

	httpServer := &http.Server{
		Addr:    ":" + cfg.HTTPPort,
		Handler: httpMux,
	}

	go func() {
		log.Printf("HTTP gateway listening on :%s", cfg.HTTPPort)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down gracefully...")
	grpcServer.GracefulStop()
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("HTTP shutdown error: %v", err)
	}
	log.Println("Server stopped")
}

func healthCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, `{"status":"ok","service":"EXBanka"}`)
}
