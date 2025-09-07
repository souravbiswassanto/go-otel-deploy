package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	serviceName             = os.Getenv("OTEL_SERVICE_NAME")
	otlpEndpoint            = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	tracer                  trace.Tracer
	meter                   metric.Meter
	httpRequestsCounter     metric.Int64Counter
	httpActiveRequests      metric.Int64UpDownCounter
	workDurationHistogram   metric.Float64Histogram
	downstreamAPIHTTPClient *http.Client
)

// initOtel sets up the OpenTelemetry pipeline.
func initOtel(ctx context.Context) (func(context.Context) error, error) {
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	conn, err := grpc.NewClient(otlpEndpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("failed to create gRPC connection to collector: %w", err)
	}

	// --- Trace Exporter ---
	traceExporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithGRPCConn(conn))
	if err != nil {
		return nil, fmt.Errorf("failed to create trace exporter: %w", err)
	}
	bsp := sdktrace.NewBatchSpanProcessor(traceExporter)
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(res),
		sdktrace.WithSpanProcessor(bsp),
	)
	otel.SetTracerProvider(tracerProvider)

	// --- Metric Exporter ---
	metricExporter, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithGRPCConn(conn))
	if err != nil {
		return nil, fmt.Errorf("failed to create metric exporter: %w", err)
	}
	reader := sdkmetric.NewPeriodicReader(metricExporter)
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(reader),
	)
	otel.SetMeterProvider(meterProvider)

	// --- Log Exporter ---
	logExporter, err := otlploggrpc.New(ctx, otlploggrpc.WithGRPCConn(conn))
	if err != nil {
		return nil, fmt.Errorf("failed to create log exporter: %w", err)
	}
	loggerProvider := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter)),
	)
	global.SetLoggerProvider(loggerProvider)

	// --- Create Tracers, Meters, and Instruments ---
	tracer = otel.Tracer("my-go-app/main-tracer")
	meter = otel.Meter("my-go-app/main-meter")

	httpRequestsCounter, err = meter.Int64Counter(
		"http.server.requests_total",
		metric.WithDescription("Total number of incoming HTTP requests."),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create http_requests_total counter: %w", err)
	}

	httpActiveRequests, err = meter.Int64UpDownCounter(
		"http.server.active_requests",
		metric.WithDescription("Number of active HTTP requests."),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create http_active_requests counter: %w", err)
	}

	workDurationHistogram, err = meter.Float64Histogram(
		"app.work.duration",
		metric.WithDescription("Duration of the work operation."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create work_duration_seconds histogram: %w", err)
	}

	// Create an instrumented HTTP client to automatically propagate trace context
	downstreamAPIHTTPClient = &http.Client{
		Transport: otelhttp.NewTransport(http.DefaultTransport),
	}

	return func(shutdownCtx context.Context) error {
		cErr := conn.Close()
		tpErr := tracerProvider.Shutdown(shutdownCtx)
		mpErr := meterProvider.Shutdown(shutdownCtx)
		lpErr := loggerProvider.Shutdown(shutdownCtx)
		if cErr != nil {
			return cErr
		}
		if tpErr != nil {
			return tpErr
		}
		if mpErr != nil {
			return mpErr
		}
		if lpErr != nil {
			return lpErr
		}
		return nil
	}, nil
}

// Middleware to count active requests
func activeRequestsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		httpActiveRequests.Add(ctx, 1)
		defer httpActiveRequests.Add(ctx, -1)
		next.ServeHTTP(w, r)
	})
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	shutdown, err := initOtel(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := shutdown(ctx); err != nil {
			log.Fatal("failed to shutdown OpenTelemetry: ", err)
		}
	}()

	mux := http.NewServeMux()
	mux.Handle("/hello", otelhttp.NewHandler(http.HandlerFunc(helloHandler), "hello"))
	mux.Handle("/work", otelhttp.NewHandler(http.HandlerFunc(workHandler), "work"))
	mux.Handle("/downstream", otelhttp.NewHandler(http.HandlerFunc(downstreamHandler), "downstream"))

	server := &http.Server{
		Addr:    ":8080",
		Handler: activeRequestsMiddleware(mux),
	}

	go func() {
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("HTTP server ListenAndServe: %v", err)
		}
	}()

	log.Println("Server started on :8080")
	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("HTTP server shutdown failed: %v", err)
	}
	log.Println("Server gracefully shutdown")
}

// Simple endpoint
func helloHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := global.Logger("helloHandler")

	_, span := tracer.Start(ctx, "helloHandler.work")
	defer span.End()

	httpRequestsCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("http.route", "/hello")))

	emitLog(ctx, logger, otellog.SeverityInfo, "Received request for /hello")

	time.Sleep(50 * time.Millisecond)
	span.AddEvent("Finished sleeping")

	fmt.Fprintln(w, "Hello, OpenTelemetry!")
}

// Endpoint that simulates work and calls a downstream service
func workHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	startTime := time.Now()
	logger := global.Logger("workHandler")

	_, span := tracer.Start(ctx, "workHandler.mainOperation")
	defer span.End()

	httpRequestsCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("http.route", "/work")))
	emitLog(ctx, logger, otellog.SeverityInfo, "Starting complex work")

	// 1. Simulate some initial work
	time.Sleep(time.Duration(75+rand.Intn(50)) * time.Millisecond)
	span.AddEvent("Initial processing complete")

	// 2. Call the downstream service
	emitLog(ctx, logger, otellog.SeverityInfo, "Calling downstream service")
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://localhost:8080/downstream", nil)

	// The instrumented client will automatically create a child span
	res, err := downstreamAPIHTTPClient.Do(req)
	if err != nil {
		http.Error(w, "Failed to call downstream service", http.StatusInternalServerError)
		emitLog(ctx, logger, otellog.SeverityError, "Downstream call failed", otellog.String("error", err.Error()))
		return
	}
	defer res.Body.Close()

	span.SetAttributes(attribute.Int("downstream.status_code", res.StatusCode))

	// 3. Simulate final processing
	time.Sleep(time.Duration(50+rand.Intn(25)) * time.Millisecond)
	span.AddEvent("Final processing complete")

	duration := time.Since(startTime).Seconds()
	workDurationHistogram.Record(ctx, duration, metric.WithAttributes(attribute.Bool("success", true)))

	emitLog(ctx, logger, otellog.SeverityInfo, "Complex work finished")
	fmt.Fprintln(w, "Work complete!")
}

// Endpoint that simulates a backend/downstream service
func downstreamHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := global.Logger("downstreamHandler")

	_, span := tracer.Start(ctx, "downstreamHandler.databaseQuery")
	defer span.End()

	httpRequestsCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("http.route", "/downstream")))
	emitLog(ctx, logger, otellog.SeverityInfo, "Downstream service received request")

	// Simulate a database query or some other backend task
	dbQueryTime := time.Duration(100+rand.Intn(150)) * time.Millisecond
	time.Sleep(dbQueryTime)

	span.SetAttributes(attribute.Float64("db.query.time_ms", float64(dbQueryTime.Milliseconds())))
	span.AddEvent("Database query finished")

	fmt.Fprintln(w, "Downstream work done.")
}

// Helper to emit logs with context
func emitLog(ctx context.Context, logger otellog.Logger, severity otellog.Severity, body string, attrs ...otellog.KeyValue) {
	record := otellog.Record{}
	record.SetTimestamp(time.Now())
	record.SetSeverity(severity)
	record.SetBody(otellog.StringValue(body))
	if len(attrs) > 0 {
		record.AddAttributes(attrs...)
	}
	logger.Emit(ctx, record)
}
