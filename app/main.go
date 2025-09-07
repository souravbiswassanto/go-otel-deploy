package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
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
	serviceName         = os.Getenv("OTEL_SERVICE_NAME")
	otlpEndpoint        = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	tracer              trace.Tracer
	meter               metric.Meter
	httpRequestsCounter metric.Int64Counter
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
	// Set up a connection to the OTLP server.
	conn, err := grpc.NewClient(otlpEndpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("failed to create gRPC connection to collector: %w", err)
	}
	// Set up a trace exporter
	traceExporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithGRPCConn(conn))
	if err != nil {
		return nil, fmt.Errorf("failed to create trace exporter: %w", err)
	}
	// Set up a meter exporter
	metricExporter, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithGRPCConn(conn))
	if err != nil {
		return nil, fmt.Errorf("failed to create metric exporter: %w", err)
	}
	// Set up a log exporter
	logExporter, err := otlploggrpc.New(ctx, otlploggrpc.WithGRPCConn(conn))
	if err != nil {
		return nil, fmt.Errorf("failed to create log exporter: %w", err)
	}
	bsp := sdktrace.NewBatchSpanProcessor(traceExporter)
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(res),
		sdktrace.WithSpanProcessor(bsp),
	)
	otel.SetTracerProvider(tracerProvider)
	reader := sdkmetric.NewPeriodicReader(metricExporter)
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(reader),
	)
	otel.SetMeterProvider(meterProvider)
	loggerProvider := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter)),
	)
	global.SetLoggerProvider(loggerProvider)
	// Create the tracer and meter
	tracer = otel.Tracer("my-go-app/main")
	meter = otel.Meter("my-go-app/main")
	httpRequestsCounter, err = meter.Int64Counter(
		"http.server.requests",
		metric.WithDescription("Number of incoming HTTP requests."),
		metric.WithUnit("1"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create http requests counter: %w", err)
	}
	return func(shutdownCtx context.Context) error {
		// Shutdown gracefully.
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
	// The otelhttp handler wraps our main handler, automatically creating spans
	handler := otelhttp.NewHandler(http.HandlerFunc(helloHandler), "hello")
	server := &http.Server{
		Addr:    ":8080",
		Handler: handler,
	}
	// Start server
	go func() {
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("HTTP server ListenAndServe: %v", err)
		}
	}()
	<-ctx.Done()
	// Shutdown server
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("HTTP server shutdown failed: %v", err)
	}
}
func helloHandler(w http.ResponseWriter, r *http.Request) {
	// Get the context from the request, which includes the parent span
	ctx := r.Context()
	// Create a child span
	_, span := tracer.Start(ctx, "helloHandler.work")
	defer span.End()
	// Increment our request counter
	httpRequestsCounter.Add(ctx, 1)
	// Create a log record using the new API
	logger := global.Logger("my-go-app/helloHandler")
	record := otellog.Record{}
	record.SetSeverity(otellog.SeverityInfo)
	record.SetBody(otellog.StringValue("Received a request for /hello"))
	// Add attributes to the record
	record.AddAttributes(
		otellog.String("http.method", r.Method),
		otellog.String("http.route", "/hello"),
		otellog.Int("request.id", 12345),
	)
	logger.Emit(ctx, record)
	// Simulate some work
	time.Sleep(100 * time.Millisecond)
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "Hello, OpenTelemetry!")
}
