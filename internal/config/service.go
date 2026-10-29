package config

import "os"

// NotifySocket reads systemd's process-scoped readiness endpoint at the
// configuration boundary. It is empty for an ordinary foreground invocation.
func NotifySocket() string { value, _ := os.LookupEnv("NOTIFY_SOCKET"); return value }
