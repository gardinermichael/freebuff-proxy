package langfuse

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdk "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func fixture(t *testing.T, capture bool) (*Exporter, *tracetest.SpanRecorder) {
	t.Helper()
	r := tracetest.NewSpanRecorder()
	p := sdk.NewTracerProvider(sdk.WithSpanProcessor(r))
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return &Exporter{provider: p, tracer: p.Tracer("test"), capture: capture, environment: "test"}, r
}
func attrs(s sdk.ReadOnlySpan) map[string]attribute.Value {
	m := map[string]attribute.Value{}
	for _, a := range s.Attributes() {
		m[string(a.Key)] = a.Value
	}
	return m
}
func TestObservationStreamAndPrivacy(t *testing.T) {
	for _, capture := range []bool{false, true} {
		t.Run(map[bool]string{false: "private", true: "content"}[capture], func(t *testing.T) {
			e, r := fixture(t, capture)
			ctx, o := e.Start(context.Background(), "/v1/messages", "requested", "req", "session", []byte(`{"messages":[{"role":"user","content":"hello"}],"authToken":"DO-NOT-EXPORT"}`))
			_, end := e.Phase(ctx, "session.acquire")
			end(nil)
			wire := "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\ndata: [DONE]\n\n"
			b, err := io.ReadAll(o.Reader(strings.NewReader(wire)))
			if err != nil || string(b) != wire {
				t.Fatal("tracing changed stream")
			}
			o.Finish("served", nil, 2)
			spans := r.Ended()
			if len(spans) != 2 {
				t.Fatal(len(spans))
			}
			root := spans[1]
			a := attrs(root)
			if spans[0].Parent().SpanID() != root.SpanContext().SpanID() {
				t.Fatal("lost hierarchy")
			}
			if root.Status().Code == codes.Error {
				t.Fatal("successful stream marked failed")
			}
			if a["langfuse.observation.model.name"].AsString() != "served" || !strings.Contains(a["langfuse.observation.usage_details"].AsString(), `"input":3`) {
				t.Fatal(a)
			}
			if a["langfuse.observation.completion_start_time"].AsString() == "" {
				t.Fatal("missing first token timestamp")
			}
			for k, v := range a {
				if strings.Contains(v.AsString(), "DO-NOT-EXPORT") {
					t.Fatal("exported credential")
				}
				if !capture && (k == "langfuse.observation.input" || k == "langfuse.observation.output") {
					t.Fatal("captured content while disabled")
				}
			}
			if capture && !strings.Contains(a["langfuse.observation.output"].AsString(), "Hi") {
				t.Fatal("missing output")
			}
		})
	}
}
func TestFailedAndTruncatedStreams(t *testing.T) {
	e, r := fixture(t, true)
	_, o := e.Start(context.Background(), "chat", "m", "r", "", nil)
	_, _ = o.Write([]byte("data: " + strings.Repeat("x", lineLimit+10) + "\n"))
	_, _ = o.Write([]byte("data: {\"error\":{\"message\":\"private upstream error\"}}\n"))
	o.Finish("m", errors.New("secret error"), 1)
	s := r.Ended()[0]
	if s.Status().Code != codes.Error {
		t.Fatal("missing failure")
	}
	if !attrs(s)["langfuse.observation.metadata.capture_truncated"].AsBool() {
		t.Fatal("missing truncation")
	}
	if len(o.line) > lineLimit {
		t.Fatal("unbounded line")
	}
}
func TestFinishConcurrentWithReader(t *testing.T) {
	e, _ := fixture(t, true)
	_, o := e.Start(context.Background(), "chat", "m", "r", "", nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			_, _ = o.Write([]byte("data: {}\n"))
		}
	}()
	o.Finish("m", context.Canceled, 1)
	<-done
}
func TestDisabledAndInvalidConfig(t *testing.T) {
	t.Setenv("LANGFUSE_ENABLED", "false")
	e, err := FromEnv(context.Background())
	if e != nil || err != nil {
		t.Fatal(e, err)
	}
	t.Setenv("LANGFUSE_ENABLED", "true")
	t.Setenv("LANGFUSE_CREDENTIALS_FILE", "")
	t.Setenv("LANGFUSE_PUBLIC_KEY", "p")
	t.Setenv("LANGFUSE_SECRET_KEY", "s")
	t.Setenv("LANGFUSE_BASE_URL", "http://example.com")
	if _, err = FromEnv(context.Background()); err == nil {
		t.Fatal("accepted insecure remote endpoint")
	}
}
func TestExporterShutdownBounded(t *testing.T) {
	e, _ := fixture(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}
