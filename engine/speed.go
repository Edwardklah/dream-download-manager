package engine

import "time"

// speedMeter measures speed over the last few seconds instead of since the
// start. An all-time average reacts too slowly: if your connection drops from
// 5 MB/s to 1 MB/s, you want to see that within a couple of seconds.
type speedMeter struct {
	window  time.Duration
	samples []sample
}

type sample struct {
	at    time.Time
	bytes int64 // total bytes downloaded at that moment
}

func newSpeedMeter(window time.Duration) *speedMeter {
	return &speedMeter{window: window}
}

// update records the current byte count and returns bytes per second.
func (m *speedMeter) update(totalBytes int64) float64 {
	now := time.Now()
	m.samples = append(m.samples, sample{now, totalBytes})

	// Drop samples older than the window.
	// GO NOTE: slices can be re-sliced: s[i:] is "everything from index i on".
	cut := 0
	for cut < len(m.samples) && now.Sub(m.samples[cut].at) > m.window {
		cut++
	}
	m.samples = m.samples[cut:]

	if len(m.samples) < 2 {
		return 0
	}
	first, last := m.samples[0], m.samples[len(m.samples)-1]
	elapsed := last.at.Sub(first.at).Seconds()
	if elapsed <= 0 {
		return 0
	}
	return float64(last.bytes-first.bytes) / elapsed
}
