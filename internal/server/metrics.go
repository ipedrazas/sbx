package server

import (
	"slices"
	"sync"
	"time"
)

const (
	ringCapacity  = 1000
	qpsWindowSecs = 60
	recentSamples = 20
)

// RequestSample is one agent-facing request recorded for the metrics panel.
type RequestSample struct {
	Timestamp int64  `json:"timestamp"`
	LatencyMS uint32 `json:"latency_ms"`
	Status    uint16 `json:"status"`
	BytesOut  uint64 `json:"bytes_out"`
}

// Metrics keeps a bounded in-memory ring of samples per dataset. Not persisted.
type Metrics struct {
	mu         sync.RWMutex
	perDataset map[int64][]RequestSample
	now        func() time.Time
}

func NewMetrics() *Metrics {
	return &Metrics{perDataset: map[int64][]RequestSample{}, now: time.Now}
}

// Record appends a sample, evicting the oldest when the ring is full.
func (m *Metrics) Record(datasetID int64, s RequestSample) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ring := m.perDataset[datasetID]
	if len(ring) == ringCapacity {
		ring = append(ring[:0:0], ring[1:]...)
	}
	m.perDataset[datasetID] = append(ring, s)
}

// DatasetMetrics is the dashboard snapshot for one dataset.
type DatasetMetrics struct {
	TotalRequests    int             `json:"total_requests"`
	ClientErrorCount int             `json:"client_error_count"`
	ServerErrorCount int             `json:"server_error_count"`
	BytesOut         uint64          `json:"bytes_out"`
	LatencyP50MS     uint32          `json:"latency_p50_ms"`
	LatencyP95MS     uint32          `json:"latency_p95_ms"`
	LatencyP99MS     uint32          `json:"latency_p99_ms"`
	LatencyMaxMS     uint32          `json:"latency_max_ms"`
	QPS60s           []uint32        `json:"qps_60s"`
	LastSeen         int64           `json:"last_seen"`
	Recent           []RequestSample `json:"recent"`
}

// Snapshot computes the dashboard stats for a dataset.
func (m *Metrics) Snapshot(datasetID int64) DatasetMetrics {
	m.mu.RLock()
	samples := slices.Clone(m.perDataset[datasetID])
	m.mu.RUnlock()
	return metricsFromSamples(samples, m.now().Unix())
}

func percentile(sorted []uint32, p int) uint32 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[p*(len(sorted)-1)/100]
}

func metricsFromSamples(samples []RequestSample, now int64) DatasetMetrics {
	out := DatasetMetrics{QPS60s: make([]uint32, qpsWindowSecs), Recent: []RequestSample{}}
	if len(samples) == 0 {
		return out
	}
	out.TotalRequests = len(samples)
	lat := make([]uint32, len(samples))
	for i, s := range samples {
		switch {
		case s.Status >= 400 && s.Status < 500:
			out.ClientErrorCount++
		case s.Status >= 500:
			out.ServerErrorCount++
		}
		out.BytesOut += s.BytesOut
		lat[i] = s.LatencyMS
		age := now - s.Timestamp
		if age >= 0 && age < qpsWindowSecs {
			out.QPS60s[qpsWindowSecs-1-age]++
		}
	}
	slices.Sort(lat)
	out.LatencyP50MS = percentile(lat, 50)
	out.LatencyP95MS = percentile(lat, 95)
	out.LatencyP99MS = percentile(lat, 99)
	out.LatencyMaxMS = lat[len(lat)-1]
	out.LastSeen = samples[len(samples)-1].Timestamp
	for i := len(samples) - 1; i >= 0 && len(out.Recent) < recentSamples; i-- {
		out.Recent = append(out.Recent, samples[i])
	}
	return out
}
