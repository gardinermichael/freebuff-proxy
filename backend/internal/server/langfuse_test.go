package server_test

import (
	"context"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/langfuse"
	"freebuff-proxy/backend/internal/server"
	"freebuff-proxy/backend/internal/testutil"
	collector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// Exercise real routing, forced upstream SSE, all client protocols, and the
// actual protobuf exporter. Assertions stay outside HTTP handlers.
func TestLangfuseAllProtocols(t *testing.T) {
	for _, tc := range []struct{ path, body string }{
		{"/v1/chat/completions", `{"model":"` + modelA + `","messages":[{"role":"user","content":"ping"}],"stream":true}`},
		{"/v1/chat/completions", `{"model":"` + modelA + `","messages":[{"role":"user","content":"ping"}]}`},
		{"/v1/messages", `{"model":"` + modelA + `","messages":[{"role":"user","content":"ping"}],"max_tokens":32,"stream":true}`},
		{"/v1/messages", `{"model":"` + modelA + `","messages":[{"role":"user","content":"ping"}],"max_tokens":32}`},
		{"/v1/responses", `{"model":"` + modelA + `","input":"ping","stream":true}`},
		{"/v1/responses", `{"model":"` + modelA + `","input":"ping"}`},
	} {
		t.Run(tc.path+tc.body, func(t *testing.T) {
			var mu sync.Mutex
			var spans []*tracepb.Span
			var exportErr error
			var path, auth, version string
			receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, err := io.ReadAll(r.Body)
				var req collector.ExportTraceServiceRequest
				if err == nil {
					err = proto.Unmarshal(b, &req)
				}
				mu.Lock()
				exportErr = err
				path = r.URL.Path
				auth = r.Header.Get("Authorization")
				version = r.Header.Get("x-langfuse-ingestion-version")
				for _, rs := range req.ResourceSpans {
					for _, ss := range rs.ScopeSpans {
						spans = append(spans, ss.Spans...)
					}
				}
				mu.Unlock()
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.WriteHeader(200)
			}))
			defer receiver.Close()
			t.Setenv("LANGFUSE_ENABLED", "true")
			t.Setenv("LANGFUSE_CREDENTIALS_FILE", "")
			t.Setenv("LANGFUSE_PUBLIC_KEY", "test-public")
			t.Setenv("LANGFUSE_SECRET_KEY", "test-secret")
			t.Setenv("LANGFUSE_BASE_URL", receiver.URL)
			t.Setenv("LANGFUSE_CAPTURE_CONTENT", "true")
			e, err := langfuse.FromEnv(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			mock := testutil.NewMock()
			mock.ChatBody = testutil.SSEEvent(chunk("trace-test", 1, `"choices":[{"index":0,"delta":{"content":"pong"},"finish_reason":null}]`)) + testutil.SSEEvent(chunk("trace-test", 1, `"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}`)) + "data: [DONE]\n\n"
			defer mock.Close()
			srv, p := server.NewTestServerStack(t, nil, []*testutil.MockUpstream{mock}, nil, nil, nil, server.WithLangfuse(e))
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				p.Shutdown(ctx)
				_ = srv.Close()
			}()
			ts := httptest.NewServer(srv.Handler())
			defer ts.Close()
			resp, b := doJSON(t, "POST", ts.URL+tc.path, []byte(tc.body), nil)
			if resp.StatusCode != 200 {
				t.Fatalf("status %d: %s", resp.StatusCode, b)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := e.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if exportErr != nil || path != "/api/public/otel/v1/traces" || version != "4" || !strings.HasPrefix(auth, "Basic ") {
				t.Fatalf("export: %v %s %s", exportErr, path, version)
			}
			var root *tracepb.Span
			for _, s := range spans {
				if len(s.ParentSpanId) == 0 {
					root = s
				}
			}
			if root == nil || len(spans) < 3 {
				t.Fatalf("missing root/acquire/attempt: %d spans", len(spans))
			}
			if hex.EncodeToString(root.TraceId) != resp.Header.Get("X-Langfuse-Trace-Id") {
				t.Fatal("trace correlation mismatch")
			}
			var generation, model, input bool
			for _, a := range root.Attributes {
				switch a.Key {
				case "langfuse.observation.type":
					generation = a.Value.GetStringValue() == "generation"
				case "langfuse.observation.model.name":
					model = a.Value.GetStringValue() == modelA
				case "langfuse.observation.input":
					input = strings.Contains(a.Value.GetStringValue(), "ping")
				}
			}
			if !generation || !model || !input {
				t.Fatalf("missing generation fields: %v %v %v", generation, model, input)
			}
			if root.Status.GetCode() == tracepb.Status_STATUS_CODE_ERROR {
				t.Fatal("success traced as error")
			}
		})
	}
}
