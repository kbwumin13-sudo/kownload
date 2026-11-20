//go:build !speedbench

package benchtrace

const Enabled = false

func Payload(_ string, _, _ int64, _ int) {}
func Terminal(_, _ string)                {}
