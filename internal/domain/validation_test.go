package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jiin-yang/notification-dispatcher/internal/domain"
)

func TestNotificationValidate(t *testing.T) {
	base := domain.Notification{
		Recipient: "+905551234567",
		Channel:   domain.ChannelSMS,
		Content:   "hello",
		Priority:  domain.PriorityNormal,
	}

	cases := []struct {
		name    string
		mutate  func(n *domain.Notification)
		wantErr error
	}{
		{"valid sms", func(*domain.Notification) {}, nil},
		{"valid email", func(n *domain.Notification) {
			n.Channel = domain.ChannelEmail
			n.Recipient = "user@example.com"
		}, nil},
		{"valid push", func(n *domain.Notification) {
			n.Channel = domain.ChannelPush
			n.Recipient = "fcm-token-abc12345"
		}, nil},
		{"empty recipient", func(n *domain.Notification) { n.Recipient = "" }, domain.ErrRecipientRequired},
		{"whitespace recipient", func(n *domain.Notification) { n.Recipient = "   " }, domain.ErrRecipientRequired},
		{"invalid channel", func(n *domain.Notification) { n.Channel = "fax" }, domain.ErrChannelInvalid},
		{"empty content", func(n *domain.Notification) { n.Content = "" }, domain.ErrContentRequired},
		{"content over limit", func(n *domain.Notification) {
			n.Content = strings.Repeat("a", domain.MaxContentLength+1)
		}, domain.ErrContentTooLong},
		{"content at limit", func(n *domain.Notification) {
			n.Content = strings.Repeat("a", domain.MaxContentLength)
		}, nil},
		{"bad priority", func(n *domain.Notification) { n.Priority = "urgent" }, domain.ErrPriorityInvalid},
		{"empty priority is ok", func(n *domain.Notification) { n.Priority = "" }, nil},
		{"sms not E.164", func(n *domain.Notification) { n.Recipient = "5551234567" }, domain.ErrRecipientFormat},
		{"email missing @", func(n *domain.Notification) {
			n.Channel = domain.ChannelEmail
			n.Recipient = "not-an-email"
		}, domain.ErrRecipientFormat},
		{"push too short", func(n *domain.Notification) {
			n.Channel = domain.ChannelPush
			n.Recipient = "short"
		}, domain.ErrRecipientFormat},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := base
			tc.mutate(&n)
			err := n.Validate()
			if tc.wantErr == nil && err != nil {
				t.Fatalf("want nil, got %v", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("want %v, got %v", tc.wantErr, err)
			}
		})
	}
}
