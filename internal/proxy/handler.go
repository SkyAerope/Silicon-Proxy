package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/SkyAerope/Silicon-Proxy/internal/pool"
)

type Handler struct {
	router         *pool.AuthRouter
	backend        *url.URL
	logger         *slog.Logger
	maxRetries     int
	requestTimeout time.Duration
}

func NewHandler(router *pool.AuthRouter, backendURL string, maxRetries int, requestTimeout time.Duration, logger *slog.Logger) (*Handler, error) {
	parsed, err := url.Parse(backendURL)
	if err != nil {
		return nil, fmt.Errorf("parse backend url failed: %w", err)
	}

	if maxRetries <= 0 {
		maxRetries = 3
	}
	if requestTimeout <= 0 {
		requestTimeout = 10 * time.Second
	}

	return &Handler{
		router:         router,
		backend:        parsed,
		logger:         logger,
		maxRetries:     maxRetries,
		requestTimeout: requestTimeout,
	}, nil
}

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	startTime := time.Now()
	baseContext := request.Context()

	authValue := strings.TrimSpace(request.Header.Get("Authorization"))
	if authValue == "" {
		http.Error(writer, "missing Authorization", http.StatusUnauthorized)
		return
	}

	bodyBytes, err := io.ReadAll(request.Body)
	if err != nil {
		http.Error(writer, "read request body failed", http.StatusBadRequest)
		return
	}
	_ = request.Body.Close()

	var lastProxy string
	var lastError error
	var authHash string

	for attempt := 1; attempt <= handler.maxRetries; attempt++ {
		remainingConnectBudget := handler.requestTimeout - time.Since(startTime)
		if remainingConnectBudget <= 0 {
			http.Error(writer, http.StatusText(http.StatusGatewayTimeout), http.StatusGatewayTimeout)
			return
		}

		resolveContext, resolveCancel := context.WithTimeout(baseContext, remainingConnectBudget)
		transport, proxyAddr, resolvedAuthHash, resolveErr := handler.router.Resolve(resolveContext, authValue)
		resolveCancel()
		authHash = resolvedAuthHash
		if resolveErr != nil {
			statusCode := http.StatusServiceUnavailable
			if errors.Is(resolveErr, context.DeadlineExceeded) || errors.Is(baseContext.Err(), context.DeadlineExceeded) {
				statusCode = http.StatusGatewayTimeout
			}
			http.Error(writer, http.StatusText(statusCode), statusCode)
			handler.logger.Warn("resolve proxy failed", "auth_hash_prefix", shortHash(authHash), "attempt", attempt, "error", resolveErr)
			return
		}

		upstreamRequest, requestErr := handler.buildUpstreamRequest(baseContext, request, bodyBytes, remainingConnectBudget)
		if requestErr != nil {
			http.Error(writer, "build upstream request failed", http.StatusBadGateway)
			return
		}

		response, roundTripErr := transport.RoundTrip(upstreamRequest)
		if roundTripErr != nil {
			lastProxy = proxyAddr
			lastError = roundTripErr
			_ = handler.router.HandleProxyFailure(baseContext, proxyAddr)
			handler.router.UnbindAuthHash(baseContext, authHash, proxyAddr)
			handler.logger.Warn(
				"proxy transport error",
				"proxy", proxyAddr,
				"auth_hash_prefix", shortHash(authHash),
				"attempt", attempt,
				"error", roundTripErr,
			)
			if isTimeoutError(roundTripErr) {
				http.Error(writer, http.StatusText(http.StatusGatewayTimeout), http.StatusGatewayTimeout)
				return
			}
			continue
		}

		handler.writeResponse(writer, response)
		handler.logger.Info(
			"request proxied",
			"method", request.Method,
			"path", request.URL.Path,
			"proxy", proxyAddr,
			"auth_hash_prefix", shortHash(authHash),
			"attempt", attempt,
			"status", response.StatusCode,
			"latency_ms", time.Since(startTime).Milliseconds(),
		)
		return
	}

	statusCode := http.StatusBadGateway
	if errors.Is(baseContext.Err(), context.DeadlineExceeded) {
		statusCode = http.StatusGatewayTimeout
	}
	http.Error(writer, http.StatusText(statusCode), statusCode)
	handler.logger.Warn(
		"all proxy retries failed",
		"auth_hash_prefix", shortHash(authHash),
		"max_retries", handler.maxRetries,
		"request_timeout", handler.requestTimeout.String(),
		"last_proxy", lastProxy,
		"error", lastError,
	)
}

func (handler *Handler) buildUpstreamRequest(baseContext context.Context, request *http.Request, bodyBytes []byte, connectBudget time.Duration) (*http.Request, error) {
	targetURL := handler.backend.ResolveReference(&url.URL{
		Path:     request.URL.Path,
		RawPath:  request.URL.RawPath,
		RawQuery: request.URL.RawQuery,
	})

	ctx := pool.WithConnectTimeout(baseContext, connectBudget)
	upstreamRequest, err := http.NewRequestWithContext(ctx, request.Method, targetURL.String(), bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}

	copyHeader(upstreamRequest.Header, request.Header)
	upstreamRequest.Host = handler.backend.Host
	return upstreamRequest, nil
}

func (handler *Handler) writeResponse(writer http.ResponseWriter, response *http.Response) {
	defer response.Body.Close()
	copyHeader(writer.Header(), response.Header)
	writer.WriteHeader(response.StatusCode)
	_, _ = io.Copy(writer, response.Body)
}

func copyHeader(dst http.Header, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}

}

func shortHash(value string) string {
	if len(value) >= 8 {
		return value[:8]
	}
	return value
}

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return false
}
