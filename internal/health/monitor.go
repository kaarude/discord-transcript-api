package health

import (
	"bufio"
	"net/http"
	"os"
	"runtime"
	runtimemetrics "runtime/metrics"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type sample struct {
	at       time.Time
	duration float64
}

type Monitor struct {
	started   time.Time
	mu        sync.Mutex
	samples   []sample
	total     atomic.Uint64
	errors    atomic.Uint64
	inFlight  atomic.Int64
	lastCPU   float64
	lastCPUAt time.Time
}

func New() *Monitor { now := time.Now(); return &Monitor{started: now, lastCPUAt: now} }

type writer struct {
	http.ResponseWriter
	status int
}

func (w *writer) WriteHeader(status int) { w.status = status; w.ResponseWriter.WriteHeader(status) }
func (w *writer) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return w.ResponseWriter.Write(data)
}

func (m *Monitor) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/health/data" || req.URL.Path == "/health/ping" || req.URL.Path == "/health/probe" {
			next.ServeHTTP(res, req)
			return
		}
		started := time.Now()
		m.inFlight.Add(1)
		wrapped := &writer{ResponseWriter: res, status: 200}
		next.ServeHTTP(wrapped, req)
		m.inFlight.Add(-1)
		m.total.Add(1)
		if wrapped.status >= 500 {
			m.errors.Add(1)
		}
		m.mu.Lock()
		m.samples = append(m.samples, sample{time.Now(), float64(time.Since(started).Microseconds()) / 1000})
		if len(m.samples) > 300 {
			m.samples = m.samples[len(m.samples)-300:]
		}
		m.mu.Unlock()
	})
}

func round(value float64) float64 { return float64(int64(value*100+.5)) / 100 }
func percentile(values []float64, target float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sort.Float64s(values)
	index := int(float64(len(values))*target+.999999) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(values) {
		index = len(values) - 1
	}
	return values[index]
}

func (m *Monitor) Snapshot() map[string]any {
	now := time.Now()
	cutoff := now.Add(-time.Minute)
	m.mu.Lock()
	values := make([]float64, 0, len(m.samples))
	rpm := 0
	for _, item := range m.samples {
		values = append(values, item.duration)
		if item.at.After(cutoff) {
			rpm++
		}
	}
	cpuNow := cpuSeconds()
	elapsed := now.Sub(m.lastCPUAt).Seconds()
	cpuPercent := 0.0
	if elapsed > 0 && m.lastCPU > 0 {
		cpuPercent = (cpuNow - m.lastCPU) / elapsed * 100
	}
	m.lastCPU = cpuNow
	m.lastCPUAt = now
	m.mu.Unlock()
	average := 0.0
	for _, value := range values {
		average += value
	}
	if len(values) > 0 {
		average /= float64(len(values))
	}
	total, failures := m.total.Load(), m.errors.Load()
	errorRate := 0.0
	if total > 0 {
		errorRate = float64(failures) * 100 / float64(total)
	}
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	p50, p95, maximum := schedulerLatency()
	load := loadAverage()
	systemUsed, systemTotal := systemMemory()
	return map[string]any{
		"status": "ok", "uptime": round(time.Since(m.started).Seconds()), "checkedAt": now.UTC().Format(time.RFC3339Nano),
		"service": map[string]any{"name": "Discord Transcript API", "version": "1.0.0", "environment": env("APP_ENV", "development"), "startedAt": m.started.UTC().Format(time.RFC3339Nano)},
		"traffic": map[string]any{"totalRequests": total, "inFlight": m.inFlight.Load(), "errors": failures, "errorRatePercent": round(errorRate), "requestsPerMinute": rpm, "averageResponseMs": round(average), "p95ResponseMs": round(percentile(values, .95)), "sampleSize": len(values)},
		"runtime": map[string]any{"cpuPercent": round(max(0.0, cpuPercent)), "eventLoopLagP50Ms": p50, "eventLoopLagP95Ms": p95, "eventLoopLagMaxMs": maximum, "loadAverage": load, "nodeVersion": runtime.Version(), "goVersion": runtime.Version(), "platform": runtime.GOOS + " " + runtime.GOARCH, "logicalCpus": runtime.NumCPU(), "goroutines": runtime.NumGoroutine()},
		"memory":  map[string]any{"rssBytes": mem.Sys, "heapUsedBytes": mem.HeapAlloc, "heapTotalBytes": mem.HeapSys, "externalBytes": mem.OtherSys, "systemUsedBytes": systemUsed, "systemTotalBytes": systemTotal, "heapUtilizationPercent": round(float64(mem.HeapAlloc) * 100 / float64(max(mem.HeapSys, 1)))},
	}
}

func cpuSeconds() float64 {
	samples := []runtimemetrics.Sample{{Name: "/cpu/classes/user:cpu-seconds"}, {Name: "/cpu/classes/gc/total:cpu-seconds"}, {Name: "/cpu/classes/scavenge/total:cpu-seconds"}}
	runtimemetrics.Read(samples)
	total := 0.0
	for _, sample := range samples {
		if sample.Value.Kind() == runtimemetrics.KindFloat64 {
			total += sample.Value.Float64()
		}
	}
	return total
}

func schedulerLatency() (float64, float64, float64) {
	sample := []runtimemetrics.Sample{{Name: "/sched/latencies:seconds"}}
	runtimemetrics.Read(sample)
	if sample[0].Value.Kind() != runtimemetrics.KindFloat64Histogram {
		return 0, 0, 0
	}
	hist := sample[0].Value.Float64Histogram()
	total := uint64(0)
	for _, count := range hist.Counts {
		total += count
	}
	quantile := func(target float64) float64 {
		if total == 0 {
			return 0
		}
		wanted := uint64(float64(total)*target + .999999)
		seen := uint64(0)
		for index, count := range hist.Counts {
			seen += count
			if seen >= wanted {
				upper := hist.Buckets[index+1]
				if upper > 1e100 {
					return 0
				}
				return round(upper * 1000)
			}
		}
		return 0
	}
	maximum := 0.0
	for index := len(hist.Counts) - 1; index >= 0; index-- {
		if hist.Counts[index] > 0 && hist.Buckets[index+1] < 1e100 {
			maximum = round(hist.Buckets[index+1] * 1000)
			break
		}
	}
	return quantile(.5), quantile(.95), maximum
}

func loadAverage() []float64 {
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return []float64{0, 0, 0}
	}
	fields := strings.Fields(string(raw))
	values := []float64{0, 0, 0}
	for index := 0; index < 3 && index < len(fields); index++ {
		values[index], _ = strconv.ParseFloat(fields[index], 64)
	}
	return values
}

func systemMemory() (uint64, uint64) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer file.Close()
	values := map[string]uint64{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 {
			number, _ := strconv.ParseUint(fields[1], 10, 64)
			values[strings.TrimSuffix(fields[0], ":")] = number * 1024
		}
	}
	total, available := values["MemTotal"], values["MemAvailable"]
	return total - available, total
}

func env(name, fallback string) string {
	if value := getenv(name); value != "" {
		return value
	}
	return fallback
}
