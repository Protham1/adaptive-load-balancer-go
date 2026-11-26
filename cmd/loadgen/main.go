// Command loadgen is a separate-process, open-loop workload generator with fault
// scheduling (Phase 13).
//
//	go run ./cmd/loadgen -scenario scenarios/flash_crowd.json -target http://localhost:8080 -out results/flash_crowd.json
//
// Properties required by the plan:
//   - fixed RPS, ramp-up, bursts, and mixed request durations (fast + long requests);
//   - open loop: arrivals follow a pre-computed seeded schedule, independent of
//     responses, so OFFERED load is recorded separately from COMPLETED throughput;
//   - fixed seed, and the scenario configuration is echoed into the output;
//   - the generator measures its own scheduling lag and dropped sends, and reports
//     generator_ok=false if it became the bottleneck.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Scenario is the on-disk scenario configuration.
type Scenario struct {
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Seed        int64        `json:"seed"`
	Path        string       `json:"request_path"`
	Arrivals    string       `json:"arrivals"` // "poisson" (default) or "uniform"
	Phases      []Phase      `json:"phases"`
	Mix         Mix          `json:"mix"`
	Faults      []FaultEvent `json:"faults"`
	TimeoutMs   int          `json:"timeout_ms"`
	MaxInFlight int          `json:"max_in_flight"`
}

// Phase is one segment of the offered-load schedule.
type Phase struct {
	Type        string  `json:"type"` // fixed | ramp | burst
	DurationS   float64 `json:"duration_s"`
	RPS         float64 `json:"rps"`      // fixed, burst (base rate)
	FromRPS     float64 `json:"from_rps"` // ramp
	ToRPS       float64 `json:"to_rps"`
	BurstRPS    float64 `json:"burst_rps"`
	BurstEveryS float64 `json:"burst_every_s"`
	BurstLenS   float64 `json:"burst_len_s"`
}

// Mix describes the request-duration mix.
type Mix struct {
	LongFraction float64 `json:"long_fraction"`
	LongDelayMs  int     `json:"long_delay_ms"`
	ShortDelayMs int     `json:"short_delay_ms"`
}

// FaultEvent applies fault settings to a backend at a scenario time.
type FaultEvent struct {
	AtS     float64        `json:"at_s"`
	Backend string         `json:"backend"`
	Set     map[string]any `json:"set"`
}

func (s *Scenario) validate() error {
	if len(s.Phases) == 0 {
		return fmt.Errorf("scenario has no phases")
	}
	total := 0.0
	for i, p := range s.Phases {
		if p.DurationS <= 0 {
			return fmt.Errorf("phase %d: duration_s must be > 0", i)
		}
		switch p.Type {
		case "fixed":
			if p.RPS < 0 || p.RPS > 50000 {
				return fmt.Errorf("phase %d: rps out of range", i)
			}
		case "ramp":
			if p.FromRPS < 0 || p.ToRPS < 0 || p.FromRPS > 50000 || p.ToRPS > 50000 {
				return fmt.Errorf("phase %d: ramp rps out of range", i)
			}
		case "burst":
			if p.RPS < 0 || p.BurstRPS < 0 || p.BurstRPS > 50000 || p.BurstEveryS <= 0 || p.BurstLenS <= 0 || p.BurstLenS > p.BurstEveryS {
				return fmt.Errorf("phase %d: invalid burst parameters", i)
			}
		default:
			return fmt.Errorf("phase %d: unknown type %q", i, p.Type)
		}
		total += p.DurationS
	}
	if s.Mix.LongFraction < 0 || s.Mix.LongFraction > 1 {
		return fmt.Errorf("mix.long_fraction must be in [0,1]")
	}
	for i, f := range s.Faults {
		if f.AtS < 0 || f.AtS > total {
			return fmt.Errorf("fault %d: at_s outside the scenario", i)
		}
		if f.Backend == "" || len(f.Set) == 0 {
			return fmt.Errorf("fault %d: backend and set are required", i)
		}
	}
	if s.Arrivals != "" && s.Arrivals != "poisson" && s.Arrivals != "uniform" {
		return fmt.Errorf("arrivals must be poisson or uniform")
	}
	return nil
}

func (s *Scenario) duration() float64 {
	t := 0.0
	for _, p := range s.Phases {
		t += p.DurationS
	}
	return t
}

// scale compresses the time axis (durations and fault times) by factor f.
func (s *Scenario) scale(f float64) {
	if f == 1 || f <= 0 {
		return
	}
	for i := range s.Phases {
		s.Phases[i].DurationS /= f
		s.Phases[i].BurstEveryS /= f
		s.Phases[i].BurstLenS /= f
	}
	for i := range s.Faults {
		s.Faults[i].AtS /= f
	}
}

// rateAt returns the offered rate (req/s) at scenario time t.
func (s *Scenario) rateAt(t float64) float64 {
	start := 0.0
	for _, p := range s.Phases {
		if t < start+p.DurationS {
			x := t - start
			switch p.Type {
			case "fixed":
				return p.RPS
			case "ramp":
				return p.FromRPS + (p.ToRPS-p.FromRPS)*x/p.DurationS
			case "burst":
				if math.Mod(x, p.BurstEveryS) < p.BurstLenS {
					return p.BurstRPS
				}
				return p.RPS
			}
		}
		start += p.DurationS
	}
	return 0
}

// Arrival is one scheduled request.
type Arrival struct {
	T       float64
	Long    bool
	DelayMs int
}

// buildSchedule pre-computes the seeded arrival schedule (open loop).
func buildSchedule(s *Scenario) []Arrival {
	rng := rand.New(rand.NewSource(s.Seed))
	total := s.duration()
	var out []Arrival
	t := 0.0
	for t < total {
		r := s.rateAt(t)
		if r <= 0 {
			t += 0.01
			continue
		}
		gap := 1.0 / r
		if s.Arrivals != "uniform" {
			gap = rng.ExpFloat64() / r
		}
		t += gap
		if t >= total {
			break
		}
		a := Arrival{T: t, DelayMs: s.Mix.ShortDelayMs}
		if rng.Float64() < s.Mix.LongFraction {
			a.Long, a.DelayMs = true, s.Mix.LongDelayMs
		}
		out = append(out, a)
	}
	return out
}

type result struct {
	sched, lag, done, lat float64
	status                int
	failed                bool
	long                  bool
}

type secBucket struct {
	T             int     `json:"t"`
	Offered       int     `json:"offered"`
	Sent          int     `json:"sent"`
	Completed     int     `json:"completed"`
	Errors        int     `json:"errors"`
	LongCompleted int     `json:"long_completed"`
	P50Ms         float64 `json:"p50_ms"`
	P95Ms         float64 `json:"p95_ms"`
	P99Ms         float64 `json:"p99_ms"`
}

type lbSample struct {
	T             float64           `json:"t"`
	Strategy      string            `json:"strategy"`
	PolicyVersion uint64            `json:"policy_version"`
	Backends      map[string]lbBack `json:"backends"`
}

type lbBack struct {
	Alive  bool   `json:"alive"`
	Active int64  `json:"active_conns"`
	Total  uint64 `json:"total_requests"`
}

type faultApplied struct {
	AtS     float64 `json:"at_s"`
	ActualS float64 `json:"actual_s"`
	Backend string  `json:"backend"`
	OK      bool    `json:"ok"`
	Error   string  `json:"error,omitempty"`
}

func pct(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(q*float64(len(sorted)))) - 1
	if i < 0 {
		i = 0
	}
	return sorted[i]
}

func parseCtrlMap(s string) map[string]string {
	m := map[string]string{"Backend-1": "http://localhost:9081", "Backend-2": "http://localhost:9082", "Backend-3": "http://localhost:9083"}
	for _, kv := range strings.Split(s, ",") {
		if p := strings.SplitN(strings.TrimSpace(kv), "=", 2); len(p) == 2 {
			m[p[0]] = p[1]
		}
	}
	return m
}

func main() {
	scPath := flag.String("scenario", "", "scenario JSON file (required)")
	target := flag.String("target", "http://localhost:8080", "load balancer base URL")
	out := flag.String("out", "", "output JSON path (default: stdout summary only)")
	seed := flag.Int64("seed", 0, "override the scenario seed (0 = use scenario)")
	scale := flag.Float64("scale", 1, "time compression factor (2 = run twice as fast)")
	ctrl := flag.String("ctrl", "", "backend control URLs, e.g. Backend-1=http://localhost:9081,Backend-2=...")
	flag.Parse()
	if *scPath == "" {
		fmt.Fprintln(os.Stderr, "usage: loadgen -scenario file.json [-target URL] [-out file] [-seed N] [-scale F]")
		os.Exit(2)
	}
	raw, err := os.ReadFile(*scPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var sc Scenario
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sc); err != nil {
		fmt.Fprintln(os.Stderr, "scenario:", err)
		os.Exit(1)
	}
	if sc.Path == "" {
		sc.Path = "/work"
	}
	if sc.TimeoutMs <= 0 {
		sc.TimeoutMs = 5000
	}
	if sc.MaxInFlight <= 0 {
		sc.MaxInFlight = 5000
	}
	if *seed != 0 {
		sc.Seed = *seed
	}
	sc.scale(*scale)
	if err := sc.validate(); err != nil {
		fmt.Fprintln(os.Stderr, "scenario:", err)
		os.Exit(1)
	}
	res := run(&sc, strings.TrimRight(*target, "/"), parseCtrlMap(*ctrl), raw, *scale)
	if *out != "" {
		b, _ := json.MarshalIndent(res, "", " ")
		if err := os.WriteFile(*out, b, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	s := res["summary"].(map[string]any)
	fmt.Printf("%s: offered=%.0f completed=%.0f errors=%.0f error_rate=%.4f p50=%.1fms p95=%.1fms p99=%.1fms generator_ok=%v\n",
		sc.Name, s["offered_total"], s["completed_total"], s["errors_total"], s["error_rate"],
		s["latency_p50_ms"], s["latency_p95_ms"], s["latency_p99_ms"], s["generator_ok"])
}

func run(sc *Scenario, target string, ctrl map[string]string, rawScenario []byte, scale float64) map[string]any {
	sched := buildSchedule(sc)
	total := sc.duration()
	client := &http.Client{
		Timeout: time.Duration(sc.TimeoutMs) * time.Millisecond,
		Transport: &http.Transport{MaxIdleConns: 2000, MaxIdleConnsPerHost: 2000, IdleConnTimeout: 30 * time.Second,
			DisableCompression: true},
	}
	var mu sync.Mutex
	var results []result
	sem := make(chan struct{}, sc.MaxInFlight)
	dropped := 0
	var wg sync.WaitGroup
	start := time.Now()

	// fault scheduler
	var faults []faultApplied
	var fwg sync.WaitGroup
	for _, f := range sc.Faults {
		f := f
		fwg.Add(1)
		go func() {
			defer fwg.Done()
			time.Sleep(time.Until(start.Add(time.Duration(f.AtS * float64(time.Second)))))
			fa := faultApplied{AtS: f.AtS, ActualS: time.Since(start).Seconds(), Backend: f.Backend}
			u, ok := ctrl[f.Backend]
			if !ok {
				fa.Error = "no control URL for backend"
			} else {
				b, _ := json.Marshal(f.Set)
				r, err := http.Post(u+"/__fault", "application/json", bytes.NewReader(b))
				if err != nil {
					fa.Error = err.Error()
				} else {
					io.Copy(io.Discard, r.Body)
					r.Body.Close()
					fa.OK = r.StatusCode == 200
					if !fa.OK {
						fa.Error = r.Status
					}
				}
			}
			mu.Lock()
			faults = append(faults, fa)
			mu.Unlock()
		}()
	}

	// load balancer sampler: routing distribution and active policy over time
	var lbs []lbSample
	stopSample := make(chan struct{})
	var swg sync.WaitGroup
	swg.Add(1)
	go func() {
		defer swg.Done()
		sc := &http.Client{Timeout: time.Second}
		tk := time.NewTicker(time.Second)
		defer tk.Stop()
		for {
			select {
			case <-stopSample:
				return
			case <-tk.C:
				s := lbSample{T: time.Since(start).Seconds(), Backends: map[string]lbBack{}}
				if r, err := sc.Get(target + "/api/status"); err == nil {
					var st struct {
						Active   string `json:"active_strategy"`
						Backends []struct {
							Name   string `json:"name"`
							Alive  bool   `json:"alive"`
							Active int64  `json:"active_conns"`
							Total  uint64 `json:"total_requests"`
						} `json:"backends"`
					}
					_ = json.NewDecoder(r.Body).Decode(&st)
					r.Body.Close()
					s.Strategy = st.Active
					for _, b := range st.Backends {
						s.Backends[b.Name] = lbBack{b.Alive, b.Active, b.Total}
					}
				}
				if r, err := sc.Get(target + "/api/policy"); err == nil {
					var p struct {
						Policy struct {
							Version uint64 `json:"version"`
						} `json:"policy"`
					}
					_ = json.NewDecoder(r.Body).Decode(&p)
					r.Body.Close()
					s.PolicyVersion = p.Policy.Version
				}
				mu.Lock()
				lbs = append(lbs, s)
				mu.Unlock()
			}
		}
	}()

	for _, a := range sched {
		due := start.Add(time.Duration(a.T * float64(time.Second)))
		if d := time.Until(due); d > 2*time.Millisecond {
			time.Sleep(d - time.Millisecond)
		}
		for time.Now().Before(due) { // final approach is a short spin for accurate scheduling
			runtime.Gosched()
		}
		select {
		case sem <- struct{}{}:
		default:
			dropped++ // generator saturated: this arrival could not be sent
			continue
		}
		wg.Add(1)
		go func(a Arrival, due time.Time) {
			defer wg.Done()
			defer func() { <-sem }()
			lag := time.Since(due).Seconds()
			url := target + sc.Path
			if a.DelayMs > 0 {
				url += fmt.Sprintf("?delay=%dms", a.DelayMs)
			}
			t1 := time.Now()
			r := result{sched: a.T, lag: lag, long: a.Long}
			resp, err := client.Get(url)
			if err != nil {
				r.failed, r.status = true, 0
			} else {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				r.status = resp.StatusCode
				r.failed = resp.StatusCode >= 500
			}
			r.lat = time.Since(t1).Seconds()
			r.done = time.Since(start).Seconds()
			mu.Lock()
			results = append(results, r)
			mu.Unlock()
		}(a, due)
	}
	wg.Wait()
	fwg.Wait()
	close(stopSample)
	swg.Wait()
	elapsed := time.Since(start).Seconds()

	// aggregate per second
	n := int(math.Ceil(math.Max(total, elapsed)))
	buckets := make([]secBucket, n+1)
	lats := make([][]float64, n+1)
	for i := range buckets {
		buckets[i].T = i
	}
	for _, a := range sched {
		buckets[int(a.T)].Offered++
	}
	var okLat, allLat, lags []float64
	errs, sent := 0, len(results)
	for _, r := range results {
		lags = append(lags, r.lag)
		si := int(r.sched)
		if si < len(buckets) {
			buckets[si].Sent++
		}
		di := int(r.done)
		if di >= len(buckets) {
			di = len(buckets) - 1
		}
		buckets[di].Completed++
		lats[di] = append(lats[di], r.lat*1000)
		allLat = append(allLat, r.lat*1000)
		if r.failed {
			buckets[di].Errors++
			errs++
		} else {
			okLat = append(okLat, r.lat*1000)
		}
		if r.long {
			buckets[di].LongCompleted++
		}
	}
	for i := range buckets {
		sort.Float64s(lats[i])
		buckets[i].P50Ms, buckets[i].P95Ms, buckets[i].P99Ms = pct(lats[i], .5), pct(lats[i], .95), pct(lats[i], .99)
	}
	sort.Float64s(okLat)
	sort.Float64s(allLat)
	sort.Float64s(lags)
	completed := len(results)
	lagP99 := pct(lags, .99) * 1000
	errRate := 0.0
	if completed > 0 {
		errRate = float64(errs) / float64(completed)
	}
	var scenarioDoc any
	_ = json.Unmarshal(rawScenario, &scenarioDoc)
	return map[string]any{
		"generator": map[string]any{
			"go": runtime.Version(), "num_cpu": runtime.NumCPU(), "gomaxprocs": runtime.GOMAXPROCS(0),
			"started_at": start.UTC().Format(time.RFC3339), "time_scale": scale, "seed": sc.Seed,
			"arrivals": orDefault(sc.Arrivals, "poisson"), "target": target,
		},
		"scenario": scenarioDoc,
		"summary": map[string]any{
			"duration_s": elapsed, "offered_total": float64(len(sched)), "sent_total": float64(sent),
			"completed_total": float64(completed), "errors_total": float64(errs), "error_rate": errRate,
			"dropped_by_generator": float64(dropped),
			"offered_rps_avg":      float64(len(sched)) / total,
			"completed_rps_avg":    float64(completed) / elapsed,
			"latency_p50_ms":       pct(allLat, .5), "latency_p95_ms": pct(allLat, .95), "latency_p99_ms": pct(allLat, .99),
			"latency_ok_p95_ms":   pct(okLat, .95),
			"schedule_lag_p50_ms": pct(lags, .5) * 1000, "schedule_lag_p99_ms": lagP99, "schedule_lag_max_ms": pct(lags, 1) * 1000,
			"generator_ok": dropped == 0 && lagP99 < 25,
		},
		"seconds": buckets, "lb": lbs, "faults_applied": faults,
	}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
