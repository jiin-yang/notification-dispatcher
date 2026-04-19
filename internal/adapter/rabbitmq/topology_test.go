package rabbitmq_test

import (
	"testing"

	"github.com/jiin-yang/notification-dispatcher/internal/adapter/rabbitmq"
)

func TestRetryQueueName(t *testing.T) {
	topo := rabbitmq.Topology{Prefix: ""}

	tests := []struct {
		channel  string
		priority string
		level    int
		want     string
	}{
		{"email", "high", 1, "email.high.retry.1"},
		{"email", "high", 2, "email.high.retry.2"},
		{"email", "high", 3, "email.high.retry.3"},
		{"email", "normal", 1, "email.normal.retry.1"},
		{"email", "normal", 2, "email.normal.retry.2"},
		{"email", "normal", 3, "email.normal.retry.3"},
		{"email", "low", 1, "email.low.retry.1"},
		{"email", "low", 2, "email.low.retry.2"},
		{"email", "low", 3, "email.low.retry.3"},
	}

	for _, tc := range tests {
		got := topo.RetryQueueName(tc.channel, tc.priority, tc.level)
		if got != tc.want {
			t.Errorf("RetryQueueName(%q, %q, %d) = %q, want %q",
				tc.channel, tc.priority, tc.level, got, tc.want)
		}
	}
}

func TestRetryQueueName_WithPrefix(t *testing.T) {
	topo := rabbitmq.Topology{Prefix: "test_"}

	got := topo.RetryQueueName("sms", "high", 2)
	want := "test_sms.high.retry.2"
	if got != want {
		t.Errorf("RetryQueueName with prefix = %q, want %q", got, want)
	}
}
