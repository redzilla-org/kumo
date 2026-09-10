package ses

import (
	"context"
	"testing"
	"time"
)

// This reproduces the web-regression race that motivated the mailbox API: the
// waiter starts before Lambda delivery and must wake on the matching message,
// without a scan/sleep loop in the caller.
func TestWaitForEmailContainingWakesOnMatchingDelivery(t *testing.T) {
	store := NewMemoryStorage()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	done := make(chan []*SentEmail, 1)
	go func() {
		messages, err := store.WaitForEmailContaining(ctx, "7e6d-test-correlation")
		if err != nil {
			t.Errorf("wait: %v", err)
		}
		done <- messages
	}()

	if _, err := store.SendEmail(context.Background(), &SentEmail{
		Destination: []string{"pat+7e6d-test-correlation@example.invalid"},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}

	messages := <-done
	if len(messages) != 1 {
		t.Fatalf("matched messages = %d, want 1", len(messages))
	}
}
