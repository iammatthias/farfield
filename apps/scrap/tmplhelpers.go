package main

import (
	"fmt"
	"time"
)

// Formatters the templates call: relative age, time-to-live, and byte sizes.

// relAge renders an RFC 3339 timestamp as compact relative age.
func relAge(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("2006-01-02")
	}
}

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

// sizeText renders a byte count for the meta line.
func sizeText(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f KB", float64(n)/1024)
}
