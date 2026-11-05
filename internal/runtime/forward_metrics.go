package runtime

import (
	"sort"
	"strconv"
	"sync"
	"time"
)

const forwardLatencyWindow = 512

// ForwardMetrics stores aggregate transport data only. No URL, header, account,
// credential, request body or response body enters the metric state.
type ForwardMetrics struct {
	Requests          uint64            `json:"requests"`
	InFlight          uint64            `json:"in_flight"`
	Completed         uint64            `json:"completed"`
	Errors            map[string]uint64 `json:"errors"`
	HTTPStatusClasses map[string]uint64 `json:"http_status_classes"`
	FirstByteSamples  int               `json:"first_byte_samples"`
	FirstByteP50MS    float64           `json:"first_byte_p50_ms"`
	FirstByteP95MS    float64           `json:"first_byte_p95_ms"`
	FirstByteP99MS    float64           `json:"first_byte_p99_ms"`
}

type forwardCounters struct {
	mu                            sync.Mutex
	requests, inFlight, completed uint64
	errors                        map[string]uint64
	status                        map[string]uint64
	latency                       [forwardLatencyWindow]float64
	latencyCount, latencyNext     int
}

func (s *forwardCounters) begin() { s.mu.Lock(); s.requests++; s.inFlight++; s.mu.Unlock() }
func (s *forwardCounters) finish(code string, status int, firstByte time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight > 0 {
		s.inFlight--
	}
	if code == "" {
		s.completed++
	} else {
		if s.errors == nil {
			s.errors = map[string]uint64{}
		}
		s.errors[code]++
	}
	if status != 0 {
		if s.status == nil {
			s.status = map[string]uint64{}
		}
		s.status[strconv.Itoa(status/100)+"xx"]++
		s.latency[s.latencyNext] = float64(firstByte) / float64(time.Millisecond)
		s.latencyNext = (s.latencyNext + 1) % forwardLatencyWindow
		if s.latencyCount < forwardLatencyWindow {
			s.latencyCount++
		}
	}
}
func (s *forwardCounters) snapshot() ForwardMetrics {
	s.mu.Lock()
	m := ForwardMetrics{Requests: s.requests, InFlight: s.inFlight, Completed: s.completed, Errors: map[string]uint64{}, HTTPStatusClasses: map[string]uint64{}, FirstByteSamples: s.latencyCount}
	for k, v := range s.errors {
		m.Errors[k] = v
	}
	for k, v := range s.status {
		m.HTTPStatusClasses[k] = v
	}
	samples := append([]float64(nil), s.latency[:s.latencyCount]...)
	s.mu.Unlock()
	if len(samples) > 0 {
		sort.Float64s(samples)
		m.FirstByteP50MS = samples[(len(samples)-1)*50/100]
		m.FirstByteP95MS = samples[(len(samples)-1)*95/100]
		m.FirstByteP99MS = samples[(len(samples)-1)*99/100]
	}
	return m
}
