// Package langfuse exports optional LLM observations through OTLP/HTTP.
// Configuration is process-environment-only and takes effect on restart.
package langfuse

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdk "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type sessionKey struct{}

type Exporter struct {
	provider    *sdk.TracerProvider
	tracer      trace.Tracer
	capture     bool
	environment string
}

// FromEnv is disabled unless explicitly enabled. Credentials are never added
// to spans or the dashboard's settings store.
func FromEnv(ctx context.Context) (*Exporter, error) {
	if os.Getenv("LANGFUSE_ENABLED") != "true" {
		return nil, nil
	}
	var c struct {
		Public string `json:"public_key"`
		Secret string `json:"secret_key"`
		URL    string `json:"base_url"`
	}
	if path := os.Getenv("LANGFUSE_CREDENTIALS_FILE"); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, errors.New("cannot read Langfuse credentials file")
		}
		if json.Unmarshal(b, &c) != nil {
			return nil, errors.New("invalid Langfuse credentials file")
		}
	}
	if v := os.Getenv("LANGFUSE_PUBLIC_KEY"); v != "" {
		c.Public = v
	}
	if v := os.Getenv("LANGFUSE_SECRET_KEY"); v != "" {
		c.Secret = v
	}
	if v := os.Getenv("LANGFUSE_BASE_URL"); v != "" {
		c.URL = v
	}
	u, err := url.Parse(c.URL)
	if err != nil {
		return nil, errors.New("invalid Langfuse base URL")
	}
	localHTTP := u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1")
	secure := u.Scheme == "https" || localHTTP
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !secure || c.Public == "" || c.Secret == "" {
		return nil, errors.New("Langfuse requires keys and an HTTPS base URL (HTTP allowed on loopback)")
	}
	exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(strings.TrimRight(c.URL, "/")+"/api/public/otel/v1/traces"), otlptracehttp.WithHeaders(map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte(c.Public+":"+c.Secret)), "x-langfuse-ingestion-version": "4"}), otlptracehttp.WithTimeout(5*time.Second))
	if err != nil {
		return nil, errors.New("cannot initialize Langfuse exporter")
	}
	p := sdk.NewTracerProvider(sdk.WithResource(resource.NewSchemaless(attribute.String("service.name", "freebuff-proxy"))), sdk.WithBatcher(exp, sdk.WithMaxQueueSize(256), sdk.WithMaxExportBatchSize(32), sdk.WithBatchTimeout(time.Second), sdk.WithExportTimeout(5*time.Second)))
	env := os.Getenv("LANGFUSE_TRACING_ENVIRONMENT")
	if env == "" {
		env = "freebuff-proxy"
	}
	return &Exporter{p, p.Tracer("freebuff-proxy"), os.Getenv("LANGFUSE_CAPTURE_CONTENT") == "true", env}, nil
}
func (e *Exporter) Shutdown(ctx context.Context) error {
	if e == nil {
		return nil
	}
	return e.provider.Shutdown(ctx)
}
func (e *Exporter) Start(ctx context.Context, endpoint, model, id, session string, body []byte) (context.Context, *Observation) {
	if e == nil {
		return ctx, nil
	}
	ctx = context.WithValue(ctx, sessionKey{}, session)
	ctx, span := e.tracer.Start(ctx, "freebuff "+endpoint, trace.WithAttributes(attribute.String("langfuse.observation.type", "generation"), attribute.String("langfuse.observation.model.name", model), attribute.String("langfuse.environment", e.environment), attribute.String("langfuse.observation.metadata.request_id", id), attribute.String("langfuse.observation.metadata.endpoint", endpoint)))
	if session != "" {
		span.SetAttributes(attribute.String("langfuse.session.id", session))
	}
	var parameters map[string]json.RawMessage
	if json.Unmarshal(body, &parameters) == nil {
		safe := map[string]json.RawMessage{}
		for _, k := range []string{"temperature", "top_p", "max_tokens", "max_completion_tokens", "max_output_tokens", "stream", "reasoning_effort"} {
			if v, ok := parameters[k]; ok && len(v) < 128 {
				safe[k] = v
			}
		}
		b, _ := json.Marshal(safe)
		span.SetAttributes(attribute.String("langfuse.observation.model.parameters", string(b)))
	}
	if e.capture {
		var obj map[string]json.RawMessage
		if json.Unmarshal(body, &obj) == nil {
			safe := map[string]json.RawMessage{}
			for _, k := range []string{"messages", "tools", "system", "input"} {
				if v, ok := obj[k]; ok {
					safe[k] = v
				}
			}
			b, _ := json.Marshal(safe)
			if len(b) <= captureLimit {
				span.SetAttributes(attribute.String("langfuse.observation.input", string(b)))
			} else {
				span.SetAttributes(attribute.Bool("langfuse.observation.metadata.input_omitted_too_large", true))
			}
		}
	}
	return ctx, &Observation{span: span, capture: e.capture}
}
func (e *Exporter) Phase(ctx context.Context, name string) (context.Context, func(error)) {
	if e == nil {
		return ctx, func(error) {}
	}
	ctx, s := e.tracer.Start(ctx, name, trace.WithAttributes(attribute.String("langfuse.observation.type", "span"), attribute.String("langfuse.environment", e.environment)))
	if session, ok := ctx.Value(sessionKey{}).(string); ok && session != "" {
		s.SetAttributes(attribute.String("langfuse.session.id", session))
	}
	return ctx, func(err error) {
		if err != nil {
			s.SetStatus(codes.Error, "upstream phase failed")
		}
		s.End()
	}
}
