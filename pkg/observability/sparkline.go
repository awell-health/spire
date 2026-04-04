package observability

import "strings"

var sparkBlocks = []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// Sparkline returns an ASCII sparkline string using block characters ▁▂▃▄▅▆▇█.
// Values are scaled to the min/max of the input slice. Returns "" if vals is empty.
func Sparkline(vals []float64) string {
	if len(vals) == 0 {
		return ""
	}
	mn, mx := vals[0], vals[0]
	for _, v := range vals {
		if v < mn {
			mn = v
		}
		if v > mx {
			mx = v
		}
	}
	rng := mx - mn
	var b strings.Builder
	for _, v := range vals {
		idx := 0
		if rng > 0 {
			idx = int((v-mn)/rng*float64(len(sparkBlocks)-1) + 0.5)
		}
		b.WriteRune(sparkBlocks[idx])
	}
	return b.String()
}
