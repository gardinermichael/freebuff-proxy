package langfuse

import (
	"bytes"
	"encoding/json"
	"io"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const captureLimit = 64 << 10
const lineLimit = 1 << 20

// Observation parses a bounded copy of upstream SSE without changing bytes or
// buffering delivery. The scanner may still be exiting on client cancellation,
// so capture and finalization share a lock.
type Observation struct {
	mu        sync.Mutex
	span      trace.Span
	capture   bool
	line      []byte
	skip      bool
	closed    bool
	finished  bool
	failed    bool
	first     time.Time
	usage     map[string]any
	output    []json.RawMessage
	size      int
	truncated bool
}

func (o *Observation) Reader(r io.Reader) io.Reader {
	if o == nil {
		return r
	}
	return io.TeeReader(r, o)
}
func (o *Observation) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return len(p), nil
	}
	for _, b := range p {
		if b == '\n' {
			if !o.skip {
				o.chunk(o.line)
			}
			o.line = o.line[:0]
			o.skip = false
			continue
		}
		if o.skip {
			continue
		}
		if len(o.line) >= lineLimit {
			o.skip = true
			o.truncated = true
			o.line = o.line[:0]
			continue
		}
		o.line = append(o.line, b)
	}
	return len(p), nil
}
func (o *Observation) chunk(line []byte) {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	data := bytes.TrimSpace(line[5:])
	if bytes.Equal(data, []byte("[DONE]")) {
		o.finished = true
		return
	}
	var c struct {
		Usage   map[string]any  `json:"usage"`
		Error   json.RawMessage `json:"error"`
		Choices []struct {
			Delta  json.RawMessage `json:"delta"`
			Finish any             `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &c) != nil {
		return
	}
	if len(c.Error) > 0 && string(c.Error) != "null" {
		o.failed = true
	}
	if c.Usage != nil {
		o.usage = c.Usage
	}
	for _, ch := range c.Choices {
		if ch.Finish != nil {
			o.finished = true
		}
		var delta map[string]any
		if json.Unmarshal(ch.Delta, &delta) != nil {
			continue
		}
		meaningful := delta["content"] != nil && delta["content"] != "" || delta["reasoning_content"] != nil && delta["reasoning_content"] != "" || delta["tool_calls"] != nil
		if !meaningful {
			continue
		}
		if o.first.IsZero() {
			o.first = time.Now()
		}
		if o.capture {
			if o.size+len(ch.Delta) <= captureLimit {
				o.output = append(o.output, append(json.RawMessage(nil), ch.Delta...))
				o.size += len(ch.Delta)
			} else {
				o.truncated = true
			}
		}
	}
}
func (o *Observation) Finish(model string, err error, attempts int) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	o.closed = true
	if len(o.line) > 0 && !o.skip {
		o.chunk(o.line)
	}
	o.span.SetAttributes(attribute.String("langfuse.observation.model.name", model), attribute.Int("langfuse.observation.metadata.attempts", attempts))
	if !o.first.IsZero() {
		o.span.SetAttributes(attribute.String("langfuse.observation.completion_start_time", o.first.UTC().Format(time.RFC3339Nano)))
	}
	if o.usage != nil {
		usage := map[string]any{}
		for src, dst := range map[string]string{"prompt_tokens": "input", "completion_tokens": "output", "total_tokens": "total"} {
			if v, ok := o.usage[src]; ok {
				usage[dst] = v
			}
		}
		b, _ := json.Marshal(usage)
		o.span.SetAttributes(attribute.String("langfuse.observation.usage_details", string(b)))
		// Preserve provider cache/reasoning breakdown without double-counting it.
		b, _ = json.Marshal(o.usage)
		o.span.SetAttributes(attribute.String("langfuse.observation.metadata.provider_usage", string(b)))
	}
	if o.capture {
		b, _ := json.Marshal(o.output)
		o.span.SetAttributes(attribute.String("langfuse.observation.output", string(b)))
	}
	if o.truncated {
		o.span.SetAttributes(attribute.Bool("langfuse.observation.metadata.capture_truncated", true))
	}
	if err != nil || o.failed || !o.finished {
		o.span.SetStatus(codes.Error, "request failed, cancelled, or incomplete")
		o.span.SetAttributes(attribute.String("langfuse.observation.level", "ERROR"))
	}
	o.span.End()
}

// FreeMode records the actual per-request USD charge, separate from Freebucks.
func (o *Observation) FreeMode(free bool) {
	if o != nil && free {
		o.span.SetAttributes(attribute.String("langfuse.observation.cost_details", `{"total":0}`))
	}
}
func (o *Observation) TraceID() string {
	if o == nil {
		return ""
	}
	return o.span.SpanContext().TraceID().String()
}
