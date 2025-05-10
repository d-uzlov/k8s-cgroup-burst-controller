package appmetrics

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	slogctx "github.com/veqryn/slog-context"
)

type InterceptHandler struct {
	ctx                 context.Context
	handler             http.Handler
	updateFunc          func(ctx context.Context) error
	updateDelay         time.Duration
	updateTimeout       time.Duration
	updateMutex         sync.Mutex
	updateLastTimestamp time.Time
}

func NewInterceptHandler(ctx context.Context, handler http.Handler, update func(ctx context.Context) error, updateDelay time.Duration, updateTimeout time.Duration) *InterceptHandler {
	return &InterceptHandler{
		ctx:           ctx,
		handler:       handler,
		updateFunc:    update,
		updateDelay:   updateDelay,
		updateTimeout: updateTimeout,
	}
}

func (h *InterceptHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if time.Since(h.updateLastTimestamp) > h.updateDelay {
		err := h.runUpdate()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		h.updateLastTimestamp = time.Now()
	}
	h.handler.ServeHTTP(w, r)
}

func (h *InterceptHandler) runUpdate() error {
	h.updateMutex.Lock()
	defer h.updateMutex.Unlock()
	handlerCtx, cancel := context.WithTimeout(h.ctx, h.updateTimeout)
	defer cancel()
	return h.updateFunc(handlerCtx)
}

func SetupMetricListener(ctx context.Context, address string, ownMetricsHandler http.Handler, containerMetricsHandler http.Handler, logHandler *slog.JSONHandler) {
	logger := slogctx.FromCtx(ctx)

	logger.Info("add default metrics handler", "address", address, "path", "/default_metrics")
	http.Handle("/default_metrics", promhttp.Handler())
	logger.Info("add own metrics handler", "address", address, "path", "/metrics")
	http.Handle("/metrics", ownMetricsHandler)
	logger.Info("add container metrics handler", "address", address, "path", "/container_metrics")
	http.Handle("/container_metrics", containerMetricsHandler)
	go func() {
		err := http.ListenAndServe(address, nil)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			panic(err.Error())
		}
	}()
}
