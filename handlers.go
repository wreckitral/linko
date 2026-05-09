package main

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"boot.dev/linko/internal/store"
	pkgerr "github.com/pkg/errors"
	"golang.org/x/crypto/bcrypt"
)

const shortURLLen = len("http://localhost:8080/") + 6

var (
	redirectsMu sync.Mutex
	redirects   []string
)

//go:embed index.html
var indexPage string

func (s *server) handlerIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	io.WriteString(w, indexPage)
}

func (s *server) handlerLogin(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (s *server) handlerShortenLink(w http.ResponseWriter, r *http.Request) {
	user, ok := r.Context().Value(UserContextKey).(string)
	if !ok || user == "" {
		httpError(r.Context(), w, http.StatusUnauthorized, errors.New("unauthorized"))
		return
	}

	longURL := r.FormValue("url")
	if longURL == "" {
		httpError(r.Context(), w, http.StatusBadRequest, errors.New("invalid URL"))
		return
	}

	u, err := url.Parse(longURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		httpError(r.Context(), w, http.StatusBadRequest, errors.New("invalid URL"))
		return
	}

	if err := checkDestination(longURL); err != nil {
		httpError(r.Context(), w, http.StatusBadRequest, errors.New("invalid URL"))
		return
	}

	shortCode, err := s.store.Create(r.Context(), longURL)
	if err != nil {
		httpError(r.Context(), w, http.StatusInternalServerError, errors.New("internal server error"))
		return
	}

	s.logger.Info("Successfully generated short code",
	"shortCode", shortCode,
	"long_url", longURL,
)
w.Header().Set("Content-Type", "text/plain")
w.WriteHeader(http.StatusCreated)
io.WriteString(w, shortCode)
}

func (s *server) handlerRedirect(w http.ResponseWriter, r *http.Request) {
	longURL, err := s.store.Lookup(r.Context(), r.PathValue("shortCode"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			httpError(r.Context(), w, http.StatusNotFound, errors.New("not found"))
		} else {
			s.logger.Error("failed to lookup URL",
			"error", err,
		)
		httpError(r.Context(), w, http.StatusInternalServerError, errors.New("internal server error"))
	}
	return
}
_, _ = bcrypt.GenerateFromPassword([]byte(longURL), bcrypt.DefaultCost)
if err := checkDestination(longURL); err != nil {
	httpError(r.Context(), w, http.StatusBadGateway, errors.New("bad gateway"))
	return
}

redirectsMu.Lock()
redirects = append(redirects, strings.Repeat(longURL, 1024))
redirectsMu.Unlock()

http.Redirect(w, r, longURL, http.StatusFound)
}

func (s *server) handlerListURLs(w http.ResponseWriter, r *http.Request) {
	codes, err := s.store.List(r.Context())
	if err != nil {
		s.logger.Error("failed to list URLs",
		"error", err,
	)
	httpError(r.Context(), w, http.StatusInternalServerError, errors.New("internal server error"))
	return
}

w.Header().Set("Content-Type", "application/json")
json.NewEncoder(w).Encode(codes)
}

func (s *server) handlerStats(w http.ResponseWriter, _ *http.Request) {
	redirectsMu.Lock()
	snapshot := redirects
	redirectsMu.Unlock()

	var bytesSaved int
	for _, u := range snapshot {
		bytesSaved += len(u) - shortURLLen
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{
		"redirects":   len(snapshot),
		"bytes_saved": bytesSaved,
	})
}

const logContextKey contextKey = "log_context"

type LogContext struct {
	Username string
	Error error
}

func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			logCtx := &LogContext{}
			ctx := context.WithValue(r.Context(), logContextKey, logCtx)
			r = r.WithContext(ctx)

			reqId := r.Header.Get("X-Request-ID")

			spyWriter := &spyResponseWriter{ResponseWriter: w}
			spyReader := &spyReadCloser{ReadCloser: r.Body}
			r.Body = spyReader
			start := time.Now()

			next.ServeHTTP(spyWriter, r)

			logArgs := []any{
				"method", r.Method,
				"path", r.URL.Path,
				"client_ip", redactIP(r.RemoteAddr),
				slog.Duration("duration", time.Since(start)),
				slog.Int("request_body_bytes", spyReader.bytesRead),
				slog.Int("response_status", spyWriter.statusCode),
				slog.Int("response_body_bytes", spyWriter.bytesWritten),
				slog.String("request_id", reqId),
			}

			if logCtx.Username != "" {
				logArgs = append(logArgs, slog.String("user", logCtx.Username))
			}

			if logCtx.Error != nil {
				logArgs = append(logArgs, slog.Any("error", logCtx.Error))
			}

			logger.Info("Served request", logArgs...)
		})
	}
}

func httpError(ctx context.Context, w http.ResponseWriter, status int, err error) {
	if logCtx, ok := ctx.Value(logContextKey).(*LogContext); ok {
		logCtx.Error = pkgerr.WithStack(err)
	}

	msg := err.Error()
	if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusInternalServerError {
		msg = http.StatusText(status)
	}

	http.Error(w, msg, status)
}

func requestId(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestId := r.Header.Get("X-Request-ID")
		if requestId == "" {
			requestId = rand.Text()
		}

		w.Header().Set("X-Request-ID", requestId)

		r.Header.Set("X-Request-ID", requestId)

		next.ServeHTTP(w, r)
	})
}

func redactIP(ipStr string) string {
	host, _, err := net.SplitHostPort(ipStr)
	if err != nil {
		return ""
	}
	ip := net.ParseIP(host).To4()

	if ip != nil {
		return fmt.Sprintf("%d.%d.%d.x", ip[0], ip[1], ip[2])
	}

	return ipStr
}

type spyResponseWriter struct {
	http.ResponseWriter
	bytesWritten int
	statusCode   int
}

func (w *spyResponseWriter) Write(p []byte) (int, error) {
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytesWritten += n
	return n, err
}

func (w *spyResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

type spyReadCloser struct {
	io.ReadCloser
	bytesRead int
}

func (r *spyReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.bytesRead += n
	return n, err
}
