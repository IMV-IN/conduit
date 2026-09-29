package server

import (
	"time"
)

func nowUTC() time.Time       { return time.Now() }
func halfLife() time.Duration { return 7 * 24 * time.Hour }
