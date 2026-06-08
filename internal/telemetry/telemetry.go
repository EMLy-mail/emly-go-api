package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Setup initialises the global OTel TracerProvider, MeterProvider and
// LoggerProvider, wires the W3C propagators, and sets the default slog logger
// to a tee that writes both to stdout (JSON) and to Loki via OTLP.
// Returns a shutdown function that must be called before process exit.
func Setup(ctx context.Context, endpoint string, stdoutHandler slog.Handler) (shutdown func(context.Context) error, err error) {
	host, insecure, err := parseEndpoint(endpoint)
	if err != nil {
		return nil, err
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName("emly-api")),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, fmt.Errorf("otel resource: %w", err)
	}

	// Traces
	traceOpts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(host), otlptracehttp.WithTimeout(5 * time.Second)}
	if insecure {
		traceOpts = append(traceOpts, otlptracehttp.WithInsecure())
	}
	traceExp, err := otlptracehttp.New(ctx, traceOpts...)
	if err != nil {
		return nil, fmt.Errorf("trace exporter: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)

	// Metrics
	metricOpts := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpoint(host), otlpmetrichttp.WithTimeout(5 * time.Second)}
	if insecure {
		metricOpts = append(metricOpts, otlpmetrichttp.WithInsecure())
	}
	metricExp, err := otlpmetrichttp.New(ctx, metricOpts...)
	if err != nil {
		_ = tp.Shutdown(ctx)
		return nil, fmt.Errorf("metric exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp,
			sdkmetric.WithInterval(15*time.Second),
		)),
	)

	// Logs
	logOpts := []otlploghttp.Option{otlploghttp.WithEndpoint(host), otlploghttp.WithTimeout(5 * time.Second)}
	if insecure {
		logOpts = append(logOpts, otlploghttp.WithInsecure())
	}
	logExp, err := otlploghttp.New(ctx, logOpts...)
	if err != nil {
		_ = tp.Shutdown(ctx)
		_ = mp.Shutdown(ctx)
		return nil, fmt.Errorf("log exporter: %w", err)
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)),
	)

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		slog.Warn("otel", "err", err)
	}))

	otelHandler := otelslog.NewHandler("emly-api", otelslog.WithLoggerProvider(lp))
	slog.SetDefault(slog.New(&teeHandler{a: stdoutHandler, b: otelHandler}))

	return func(ctx context.Context) error {
		if err := tp.Shutdown(ctx); err != nil {
			return err
		}
		if err := mp.Shutdown(ctx); err != nil {
			return err
		}
		return lp.Shutdown(ctx)
	}, nil
}

// parseEndpoint splits a raw URL like "http://host:port" into host:port and
// whether the connection is plain HTTP (insecure).
func parseEndpoint(rawURL string) (hostPort string, insecure bool, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", false, fmt.Errorf("invalid otel endpoint %q: %w", rawURL, err)
	}
	return u.Host, u.Scheme == "http", nil
}

// teeHandler fans out slog records to two handlers simultaneously.
type teeHandler struct {
	a, b slog.Handler
}

func (h *teeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.a.Enabled(ctx, l) || h.b.Enabled(ctx, l)
}

func (h *teeHandler) Handle(ctx context.Context, r slog.Record) error {
	_ = h.a.Handle(ctx, r)
	return h.b.Handle(ctx, r)
}

func (h *teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &teeHandler{a: h.a.WithAttrs(attrs), b: h.b.WithAttrs(attrs)}
}

func (h *teeHandler) WithGroup(name string) slog.Handler {
	return &teeHandler{a: h.a.WithGroup(name), b: h.b.WithGroup(name)}
}
