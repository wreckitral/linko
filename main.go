package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"boot.dev/linko/internal/store"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	httpPort := flag.Int("port", 8899, "port to listen on")
	dataDir := flag.String("data", "./data", "directory to store data")
	flag.Parse()

	status := run(ctx, cancel, *httpPort, *dataDir)
	cancel()

	os.Exit(status)
}

func run(ctx context.Context, cancel context.CancelFunc, httpPort int, dataDir string) int {
	accessLoggerFile, err := os.OpenFile("linko.access.log", os.O_RDWR | os.O_CREATE | os.O_APPEND, 0666)
	if err != nil {
		log.Printf("failed to open logger file: %s", err)
	}
	defer accessLoggerFile.Close()

	accessLogger := log.New(accessLoggerFile, "INFO: ", log.LstdFlags)
	standardLogger := log.New(os.Stderr, "DEBUG: ", log.LstdFlags)

	st, err := store.New(dataDir, standardLogger)
	if err != nil {
		standardLogger.Printf("failed to create store: %v", err)
		return 1
	}
	s := newServer(*st, httpPort, cancel, accessLogger)
	var serverErr error
	go func() {
		serverErr = s.start()
	}()

	standardLogger.Printf("Linko is running on http://localhost:%d", httpPort)

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	standardLogger.Print("Linko is shutting down")
	defer cancel()

	if err := s.shutdown(shutdownCtx); err != nil {
		standardLogger.Printf("failed to shutdown server: %v\n", err)
		return 1
	}
	if serverErr != nil {
		accessLogger.Printf("server error: %v\n", serverErr)
		return 1
	}
	return 0
}
