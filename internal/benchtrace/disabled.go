//go:build !speedbench

package benchtrace

import "time"

const Enabled = false

func Payload(_ string, _, _ int64, _ int) {}
func Terminal(_, _, _ string)             {}
func Retry(_ string)                      {}
func Deferral(_ string)                   {}
func Budget(_, _ string, _, _ time.Time)  {}
