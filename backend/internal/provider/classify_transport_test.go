package provider

import (
	"context"
	"errors"
	"testing"
)

func TestClassifyTransportError(t *testing.T) {
	t.Run("canceled context is client disconnect", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if got := ClassifyTransportError(ctx, context.Canceled); got != ErrorTypeClientDisconnected {
			t.Fatalf("ClassifyTransportError(canceled) = %q, want %q", got, ErrorTypeClientDisconnected)
		}
	})

	t.Run("wrapped canceled error is client disconnect", func(t *testing.T) {
		err := errors.Join(errors.New("transport stopped"), context.Canceled)
		if got := ClassifyTransportError(context.Background(), err); got != ErrorTypeClientDisconnected {
			t.Fatalf("ClassifyTransportError(wrapped canceled) = %q, want %q", got, ErrorTypeClientDisconnected)
		}
	})

	t.Run("deadline is timeout", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 0)
		defer cancel()
		<-ctx.Done()

		if got := ClassifyTransportError(ctx, ctx.Err()); got != ErrorTypeTimeout {
			t.Fatalf("ClassifyTransportError(deadline) = %q, want %q", got, ErrorTypeTimeout)
		}
	})

	t.Run("other transport error is connection", func(t *testing.T) {
		if got := ClassifyTransportError(context.Background(), errors.New("connection reset")); got != ErrorTypeConnection {
			t.Fatalf("ClassifyTransportError(connection) = %q, want %q", got, ErrorTypeConnection)
		}
	})
}

func TestShouldReportKeyPool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ShouldReportKeyPool(ctx, ErrorTypeConnection) {
		t.Fatal("canceled parent context must not update key pool")
	}

	deadlineCtx, deadlineCancel := context.WithTimeout(context.Background(), 0)
	defer deadlineCancel()
	<-deadlineCtx.Done()
	if ShouldReportKeyPool(deadlineCtx, ErrorTypeTimeout) {
		t.Fatal("expired parent deadline must not update key pool")
	}

	if ShouldReportKeyPool(context.Background(), ErrorTypeClientDisconnected) {
		t.Fatal("client disconnect must not update key pool")
	}
	if !ShouldReportKeyPool(context.Background(), ErrorTypeConnection) {
		t.Fatal("active transport failure should update key pool")
	}
}
