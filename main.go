package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	linkoerr "boot.dev/linko/internal"
	"github.com/lmittmann/tint"
	"github.com/mattn/go-isatty"
	pkgerr "github.com/pkg/errors"
	"gopkg.in/natefinch/lumberjack.v2"

	"boot.dev/linko/internal/build"
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
	logger, closeLogger, err := initializeLogger(os.Getenv("LINKO_LOG_FILE"))
	env := os.Getenv("ENV")
	hostname, err := os.Hostname()
	if err != nil {
		logger.Error(fmt.Sprintf("failed to get hostname: %v", err))
		return 1
	}

	logger = logger.With(
		slog.String("git_sha", build.GitSHA),
		slog.String("build_time", build.BuildTime),
		slog.String("env", env),
		slog.String("hostname", hostname),
	)

	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize logger: %v\n", err)
		return 1
	}
	defer func() {
		if err := closeLogger(); err != nil {
			fmt.Fprintf(os.Stderr, "logger cleanup error: %v\n", err)
		}
	}()

	st, err := store.New(dataDir, logger)
	if err != nil {
		logger.Error(fmt.Sprintf("failed to create store: %v", err))
		return 1
	}
	s := newServer(*st, httpPort, cancel, logger)
	var serverErr error
	go func() {
		serverErr = s.start()
	}()

	logger.Debug(fmt.Sprintf("Linko is running on http://localhost:%d", httpPort))

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	logger.Debug("Linko is shutting down")
	defer cancel()

	if err := s.shutdown(shutdownCtx); err != nil {
		logger.Error(fmt.Sprintf("failed to shutdown server: %v\n", err))
		return 1
	}
	if serverErr != nil {
		logger.Error(fmt.Sprintf("server error: %v\n", serverErr))
		return 1
	}
	return 0
}

type closeFunc func() error

func initializeLogger(logFile string) (*slog.Logger, closeFunc, error) {
	var (
		handlers []slog.Handler
		closers []closeFunc
	)

	fd := os.Stdout.Fd()
	isTTY := isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd)

	handlers = append(handlers, tint.NewHandler(os.Stderr, &tint.Options{
		ReplaceAttr: replaceAttr,
		NoColor: !isTTY,
	}))

	if logFile != ""  {
		logger := &lumberjack.Logger{
			Filename:   logFile,
			MaxSize:    1,
			MaxAge:     28,
			MaxBackups: 10,
			LocalTime:  false,
			Compress:   true,
		}

		handlers = append(handlers, slog.NewJSONHandler(logger, &slog.HandlerOptions{
			ReplaceAttr: replaceAttr,
		}))
		closers = append(closers, func() error {
			if err := logger.Close(); err != nil {
				return err
			}

			return nil
		})
	}

	close := func() error {
		var errs []error
		for _, closer := range closers {
			errs = append(errs, closer())
		}
		return errors.Join(errs...)
	}

	return slog.New(slog.NewMultiHandler(handlers...)), close, nil
}

type stackTracer interface {
	error
	StackTrace() pkgerr.StackTrace
}

type multiError interface {
	error
	Unwrap() []error
}

func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if a.Key == "error" {
		errObj, ok := a.Value.Any().(error)
		if !ok {
			return a
		}

		var multiErr multiError
		if errors.As(errObj, &multiErr) {
			unwrappedErrs := multiErr.Unwrap()

			var multiGroupArgs []slog.Attr

			for i, singleErr := range unwrappedErrs {
				extractedAttrs := errorAttrs(singleErr)

				errorKey := fmt.Sprintf("error_%d", i+1)

				multiGroupArgs = append(multiGroupArgs, slog.Attr{
					Key:   errorKey,
					Value: slog.GroupValue(extractedAttrs...),
				})
			}

			return slog.Attr{
				Key:   "errors",
				Value: slog.GroupValue(multiGroupArgs...),
			}
		}

		extractedAttrs := errorAttrs(errObj)
		return slog.Attr{
			Key:   "error",
			Value: slog.GroupValue(extractedAttrs...),
		}
	}

	return a
}

func errorAttrs(errObj error) []slog.Attr {
	// 1. Always grab the base error message
	attrs := []slog.Attr{
		slog.String("message", errObj.Error()),
	}

	// 2. Check for a stack trace
	var stackErr stackTracer
	if errors.As(errObj, &stackErr) {
		attrs = append(attrs, slog.String("stack_trace", fmt.Sprintf("%+v", stackErr.StackTrace())))
	}

	// 3. Extract any custom attributes (like "path": "data/bad_entry")
	customAttrs := linkoerr.Attrs(errObj)
	attrs = append(attrs, customAttrs...)

	return attrs
}
