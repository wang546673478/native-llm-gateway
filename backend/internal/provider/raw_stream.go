package provider

import (
	"bytes"
	"context"
	"io"
)

const (
	rawStreamBufferSize = 32 * 1024
	defaultMaxSSEEvent  = 1 << 20
)

// ForwardRawStream copies response body bytes into StreamChunk without SSE
// decoding or re-encoding. observe is best-effort accounting only and cannot
// alter the bytes sent to the consumer.
func ForwardRawStream(
	ctx context.Context,
	body io.ReadCloser,
	observe func([]byte),
	finish func(),
) <-chan *StreamChunk {
	ch := make(chan *StreamChunk, 16)
	go func() {
		defer close(ch)
		defer body.Close()
		if finish != nil {
			defer finish()
		}

		buf := make([]byte, rawStreamBufferSize)
		for {
			n, err := body.Read(buf)
			if n > 0 {
				data := append([]byte(nil), buf[:n]...)
				if observe != nil {
					observe(data)
				}
				if !SendOrAbort(ctx, ch, &StreamChunk{Data: data}) {
					return
				}
			}
			if err != nil {
				SendOrAbort(ctx, ch, &StreamChunk{Err: err})
				return
			}
		}
	}()
	return ch
}

// SSEEventObserver frames a copy of raw SSE bytes for passive usage/error
// parsing. It never participates in the response path. Oversized events are
// discarded until their blank-line delimiter so observation stays bounded.
type SSEEventObserver struct {
	MaxEventBytes int
	OnEvent       func([]byte)

	pending    []byte
	event      []byte
	discarding bool
}

func (o *SSEEventObserver) Observe(data []byte) {
	if len(data) == 0 {
		return
	}
	o.pending = append(o.pending, data...)
	o.consume(false)
}

func (o *SSEEventObserver) Finish() {
	o.consume(true)
	if !o.discarding && len(o.event) > 0 {
		o.emit()
	}
	o.pending = nil
	o.event = nil
	o.discarding = false
}

func (o *SSEEventObserver) consume(final bool) {
	limit := o.MaxEventBytes
	if limit <= 0 {
		limit = defaultMaxSSEEvent
	}

	for {
		newline := bytes.IndexByte(o.pending, '\n')
		if newline < 0 {
			if final && len(o.pending) > 0 {
				o.consumeLine(o.pending, limit)
				o.pending = nil
			} else if len(o.pending) > limit {
				o.pending = append([]byte(nil), o.pending[len(o.pending)-1:]...)
				o.event = nil
				o.discarding = true
			}
			return
		}
		line := o.pending[:newline+1]
		o.pending = o.pending[newline+1:]
		o.consumeLine(line, limit)
	}
}

func (o *SSEEventObserver) consumeLine(line []byte, limit int) {
	if len(bytes.Trim(line, "\r\n")) == 0 {
		if !o.discarding && len(o.event) > 0 {
			o.emit()
		}
		o.event = o.event[:0]
		o.discarding = false
		return
	}
	if o.discarding {
		return
	}
	if len(o.event)+len(line) > limit {
		o.event = o.event[:0]
		o.discarding = true
		return
	}
	o.event = append(o.event, line...)
}

func (o *SSEEventObserver) emit() {
	if o.OnEvent != nil {
		o.OnEvent(append([]byte(nil), o.event...))
	}
	o.event = o.event[:0]
}
