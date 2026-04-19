package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const MaxContentLength = 500

var (
	ErrRecipientRequired = errors.New("recipient is required")
	ErrChannelInvalid    = errors.New("channel must be one of: sms, email, push")
	ErrContentRequired   = errors.New("content is required")
	ErrContentTooLong    = fmt.Errorf("content exceeds %d characters", MaxContentLength)
	ErrRecipientFormat   = errors.New("recipient format invalid for channel")
	ErrPriorityInvalid   = errors.New("priority must be one of: high, normal, low")
)

// E.164: leading '+' and 8–15 digits. Permissive on purpose.
var phoneRegex = regexp.MustCompile(`^\+[1-9]\d{7,14}$`)

// Intentionally simple email check. Not RFC 5322 complete; sufficient for
// boundary validation. Provider will reject properly malformed addresses.
var emailRegex = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// Validate enforces the Phase 1 input contract. Priority is validated only
// when non-empty; callers set a default before calling.
func (n Notification) Validate() error {
	if strings.TrimSpace(n.Recipient) == "" {
		return ErrRecipientRequired
	}
	if !n.Channel.Valid() {
		return ErrChannelInvalid
	}
	if strings.TrimSpace(n.Content) == "" {
		return ErrContentRequired
	}
	if utf8.RuneCountInString(n.Content) > MaxContentLength {
		return ErrContentTooLong
	}
	if n.Priority != "" && !n.Priority.Valid() {
		return ErrPriorityInvalid
	}
	if err := validateRecipient(n.Channel, n.Recipient); err != nil {
		return err
	}
	return nil
}

func validateRecipient(ch Channel, recipient string) error {
	switch ch {
	case ChannelSMS:
		if !phoneRegex.MatchString(recipient) {
			return fmt.Errorf("%w: sms recipient must be E.164 (e.g. +905551234567)", ErrRecipientFormat)
		}
	case ChannelEmail:
		if !emailRegex.MatchString(recipient) {
			return fmt.Errorf("%w: email recipient must contain a valid address", ErrRecipientFormat)
		}
	case ChannelPush:
		// Device tokens vary by vendor; only enforce non-empty + length bound.
		if len(recipient) < 8 || len(recipient) > 4096 {
			return fmt.Errorf("%w: push recipient must be 8–4096 chars", ErrRecipientFormat)
		}
	}
	return nil
}
