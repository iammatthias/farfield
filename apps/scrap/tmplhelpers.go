package main

import (
	"fmt"
	"time"
)

// Formatters the templates call: relative age, time-to-live, and byte sizes.

// ttlText renders an expiry deadline as a server-side countdown (” = never).
func ttlText(expiresAt string) string {
	if expiresAt == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return ""
	}
	d := time.Until(t)
	switch {
	case d <= 0:
		return "expired"
	case d < time.Hour:
		return fmt.Sprintf("expires in %dm", int(d.Minutes())+1)
	case d < 48*time.Hour:
		return fmt.Sprintf("expires in %dh", int(d.Hours())+1)
	default:
		return fmt.Sprintf("expires in %dd", int(d.Hours()/24)+1)
	}
}
