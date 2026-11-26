// Command loadgen is a separate-process, open-loop workload generator with fault
// scheduling and measurement export (Phases 13-14).
//
//	go run ./cmd/loadgen -scenario experiments/scenarios/e6_flash_crowd.json -target http://localhost:8080 -out run/loadgen.json
//
// Properties required by the plan:
//   - fixed RPS, ramp-up, bursts, and mixed request durations (fast + long requests);
//   - open loop: arrivals follow a pre-computed seeded schedule, independent of
//     responses, so OFFERED load is recorded separately from COMPLETED throughput;
//   - fixed seed, and the scenario configuration is echoed into the output;
//   - the generator measures its own scheduling lag and dropped sends, and reports
//     generator_ok=false if it became the bottleneck.
//
// Measurement export (kept small and privacy-safe: no request bodies, no real
// client identities):
//   - windows: per-window counters and a LOG-BUCKET LATENCY HISTOGRAM of
//     successful requests (one methodology for every percentile; see histogram below);
//   - failures are categorised: http_5xx, http_4xx, timeout, transport;
//   - optional sampling (-sample, -samples-out) of individual requests;
//   - optional affinity tracking for workloads with synthetic client ids.
package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Scenario is the on-disk scenario configuration.
type Scenario struct {
	Name         string       `json:"name"`
	Description  string       `json:"description"`
	Seed         int64        `json:"seed"`
	Path         string       `json:"request_path"`
	Arrivals     string       `json:"arrivals"` // "poisson" (default) or "uniform"
	Phases       []Phase      `json:"phases"`
	Mix          Mix          `json:"mix"`
	Faults       []FaultEvent `json:"faults"`
	TimeoutMs    int          `json:"timeout_ms"`
	MaxInFlight  int          `json:"max_in_flight"`
	TrackBackend bool         `json:"track_backend"` // read the responding backend's name from the body
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

// Mix describes the request-duration mix and (optionally) synthetic clients.
type Mix struct {
	LongFraction float64 `json:"long_fraction"`
	LongDelayMs  int     `json:"long_delay_ms"`
	ShortDelayMs int     `json:"short_delay_ms"`
	Clients      int     `json:"clients"` // >0: requests carry a synthetic X-Forwarded-For (affinity workload)
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
	if s.Mix.Clients < 0 || s.Mix.Clients > 1000000 {
		return fmt.Errorf("mix.clients out of range")
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
	Client  int  // synthetic client id (only if mix.clients > 0)
	Sample  bool // selected for the request-sample file
}

// buildSchedule pre-computes the seeded arrival schedule (open loop). Clients and
// sampling use separate generators so that enabling them never changes the
// arrival times or the long/short mix of a scenario.
func buildSchedule(s *Scenario, sampleRate float64) []Arrival {
	rng := rand.New(rand.NewSource(s.Seed))
	crng := rand.New(rand.NewSource(s.Seed + 7919))
	srng := rand.New(rand.NewSource(s.Seed + 104729))
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
		if s.Mix.Clients > 0 {
			a.Client = crng.Intn(s.Mix.Clients) + 1
		}
		if sampleRate > 0 && srng.Float64() < sampleRate {
			a.Sample = true
		}
		out = append(out, a)
	}
	return out
}

// ---------------------------------------------------------------- histogram
// Latency methodology: successful (2xx/3xx) request latencies are recorded in a
// fixed log-bucket histogram, bucket i covering (histBaseMs*r^(i-1), histBaseMs*r^i]
// with r = histRatio (5%). A percentile is reported as the UPPER bound of the
// bucket containing it, i.e. a conservative estimate within +5% of the true value.
// Histograms merge by addition, so any window/run/policy aggregation uses the same
// method (percentiles are never averaged).
const (
	histBaseMs = 0.1
	histRatio  = 1.05
	histMaxMs  = 120000.0
)

var histBuckets = int(math.Ceil(math.Log(histMaxMs/histBaseMs)/math.Log(histRatio))) + 1

func histIndex(ms float64) int {
	if ms <= histBaseMs {
		return 0
	}
	i := int(math.Ceil(math.Log(ms/histBaseMs) / math.Log(histRatio)))
	if i >= histBuckets {
		i = histBuckets - 1
	}
	return i
}

type result struct {
	sched, lag, done, lat float64
	status                int
	category              string // "" = success
	long                  bool
	backend               string
	client                int
	sample                bool
}

// Window is one export window (completion-time based for completions, schedule-time for offered).
type Window struct {
	T0            float64        `json:"t0"`
	T1            float64        `json:"t1"`
	Offered       int            `json:"offered"`
	Sent          int            `json:"sent"`
	Completed     int            `json:"completed"`
	OK            int            `json:"ok"`
	Failed        int            `json:"failed"`
	Err5xx        int            `json:"err_5xx"`
	Err4xx        int            `json:"err_4xx"`
	ErrTimeout    int            `json:"err_timeout"`
	ErrTransport  int            `json:"err_transport"`
	LatSumOKMs    float64        `json:"lat_sum_ok_ms"`
	HistOK        map[string]int `json:"hist_ok"` // sparse: bucket index -> count
	LongCompleted int            `json:"long_completed"`
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
	SwitchCount   uint64            `json:"switch_count"`
	OverheadP50Ms float64           `json:"overhead_p50_ms"`
	OverheadP95Ms float64           `json:"overhead_p95_ms"`
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

// parseBackendName extracts "Backend-2" from "Response from Backend-2 (Port 8082) | ...".
func parseBackendName(body []byte) string {
	const pre = "Response from "
	i := bytes.Index(body, []byte(pre))
	if i < 0 {
		return ""
	}
	rest := body[i+len(pre):]
	j := bytes.IndexByte(rest, ' ')
	if j < 0 {
		return string(rest)
	}
	return string(rest[:j])
}

func syntheticIP(c int) string { return fmt.Sprintf("10.%d.%d.%d", (c>>16)&255, (c>>8)&255, c&255) }

func categorize(status int, err error) string {
	if err != nil {
		var ne net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
			return "timeout"
		}
		return "transport"
	}
	switch {
	case status >= 500:
		return "http_5xx"
	case status >= 400:
		return "http_4xx"
	}
	return ""
}

func main() {
	scPath := flag.String("scenario", "", "scenario JSON file (required)")
	target := flag.String("target", "http://localhost:8080", "load balancer base URL")
	out := flag.String("out", "", "output JSON path (default: stdout summary only)")
	seed := flag.Int64("seed", 0, "override the scenario seed (0 = use scenario)")
	scale := flag.Float64("scale", 1, "time compression factor (2 = run twice as fast)")
	ctrl := flag.String("ctrl", "", "backend control URLs, e.g. Backend-1=http://localhost:9081,Backend-2=...")
	window := flag.Float64("window", 5, "export window length in seconds")
	sample := flag.Float64("sample", 0, "fraction of requests to sample into -samples-out (0 = none)")
	samplesOut := flag.String("samples-out", "", "CSV path for sampled requests")
	validateOnly := flag.Bool("validate", false, "validate the scenario file and exit")
	flag.Parse()
	if *scPath == "" {
		fmt.Fprintln(os.Stderr, "usage: loadgen -scenario file.json [-target URL] [-out file] [-seed N] [-scale F] [-window S] [-sample F -samples-out file]")
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
	if *sample < 0 || *sample > 1 || *window <= 0 {
		fmt.Fprintln(os.Stderr, "invalid -sample or -window")
		os.Exit(2)
	}
	if *validateOnly {
		fmt.Println("ok")
		return
	}
	res, samples := run(&sc, strings.TrimRight(*target, "/"), parseCtrlMap(*ctrl), raw, *scale, *window, *sample)
	if *out != "" {
		b, _ := json.MarshalIndent(res, "", " ")
		if err := os.WriteFile(*out, b, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	if *samplesOut != "" {
		if err := writeSamples(*samplesOut, samples); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	s := res["summary"].(map[string]any)
	fmt.Printf("%s: offered=%.0f completed=%.0f errors=%.0f error_rate=%.4f p50=%.1fms p95=%.1fms p99=%.1fms generator_ok=%v\n",
		sc.Name, s["offered_total"], s["completed_total"], s["errors_total"], s["error_rate"],
		s["latency_p50_ms"], s["latency_p95_ms"], s["latency_p99_ms"], s["generator_ok"])
}

func writeSamples(path string, rs []result) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	_ = w.Write([]string{"sched_s", "latency_ms", "status", "category", "backend", "long"})
	for _, r := range rs {
		_ = w.Write([]string{strconv.FormatFloat(r.sched, 'f', 3, 64), strconv.FormatFloat(r.lat*1000, 'f', 3, 64),
			strconv.Itoa(r.status), r.category, r.backend, strconv.FormatBool(r.long)})
	}
	w.Flush()
	return w.Error()
}

func run(sc *Scenario, target string, ctrl map[string]string, rawScenario []byte, scale, windowS, sampleRate float64) (map[string]any, []result) {
	sched := buildSchedule(sc, sampleRate)
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
	affinity := map[int]map[string]int{}
	needBackend := sc.TrackBackend || sampleRate > 0

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

	// load balancer sampler: routing distribution, policy, switch count and request-path overhead
	var lbs []lbSample
	stopSample := make(chan struct{})
	var swg sync.WaitGroup
	swg.Add(1)
	go func() {
		defer swg.Done()
		hc := &http.Client{Timeout: time.Second}
		tk := time.NewTicker(time.Second)
		defer tk.Stop()
		for {
			select {
			case <-stopSample:
				return
			case <-tk.C:
				s := lbSample{T: time.Since(start).Seconds(), Backends: map[string]lbBack{}}
				if r, err := hc.Get(target + "/api/status"); err == nil {
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
				if r, err := hc.Get(target + "/api/policy"); err == nil {
					var p struct {
						Policy struct {
							Version uint64 `json:"version"`
						} `json:"policy"`
					}
					_ = json.NewDecoder(r.Body).Decode(&p)
					r.Body.Close()
					s.PolicyVersion = p.Policy.Version
				}
				if r, err := hc.Get(target + "/api/metrics"); err == nil {
					var m struct {
						Controller struct {
							SwitchCount uint64 `json:"switch_count"`
						} `json:"controller"`
						Global struct {
							Overhead struct {
								P50 float64 `json:"p50_ms"`
								P95 float64 `json:"p95_ms"`
							} `json:"overhead_duration_stats"`
						} `json:"global_traffic"`
					}
					_ = json.NewDecoder(r.Body).Decode(&m)
					r.Body.Close()
					s.SwitchCount = m.Controller.SwitchCount
					s.OverheadP50Ms, s.OverheadP95Ms = m.Global.Overhead.P50, m.Global.Overhead.P95
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
			req, err := http.NewRequest(http.MethodGet, url, nil)
			if err != nil {
				return
			}
			if a.Client > 0 {
				req.Header.Set("X-Forwarded-For", syntheticIP(a.Client))
			}
			t1 := time.Now()
			r := result{sched: a.T, lag: lag, long: a.Long, client: a.Client, sample: a.Sample}
			resp, err := client.Do(req)
			if err == nil {
				if needBackend {
					buf := make([]byte, 128)
					n, _ := io.ReadFull(resp.Body, buf)
					r.backend = parseBackendName(buf[:n])
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				r.status = resp.StatusCode
			}
			r.category = categorize(r.status, err)
			r.lat = time.Since(t1).Seconds()
			r.done = time.Since(start).Seconds()
			mu.Lock()
			results = append(results, r)
			if a.Client > 0 && r.backend != "" {
				m := affinity[a.Client]
				if m == nil {
					m = map[string]int{}
					affinity[a.Client] = m
				}
				m[r.backend]++
			}
			mu.Unlock()
		}(a, due)
	}
	wg.Wait()
	fwg.Wait()
	close(stopSample)
	swg.Wait()
	elapsed := time.Since(start).Seconds()

	// ---- aggregate: per-second buckets (for adaptation analysis) and export windows
	n := int(math.Ceil(math.Max(total, elapsed)))
	buckets := make([]secBucket, n+1)
	lats := make([][]float64, n+1)
	for i := range buckets {
		buckets[i].T = i
	}
	nw := int(math.Ceil(math.Max(total, elapsed)/windowS)) + 1
	wins := make([]Window, nw)
	for i := range wins {
		wins[i] = Window{T0: float64(i) * windowS, T1: float64(i+1) * windowS, HistOK: map[string]int{}}
	}
	for _, a := range sched {
		buckets[int(a.T)].Offered++
		wins[int(a.T/windowS)].Offered++
	}
	var okLat, allLat, lags []float64
	cats := map[string]int{}
	errs := 0
	var samples []result
	for _, r := range results {
		lags = append(lags, r.lag)
		if si := int(r.sched); si < len(buckets) {
			buckets[si].Sent++
		}
		if wi := int(r.sched / windowS); wi < nw {
			wins[wi].Sent++
		}
		di := int(r.done)
		if di >= len(buckets) {
			di = len(buckets) - 1
		}
		wi := int(r.done / windowS)
		if wi >= nw {
			wi = nw - 1
		}
		buckets[di].Completed++
		wins[wi].Completed++
		allLat = append(allLat, r.lat*1000)
		if r.long {
			buckets[di].LongCompleted++
			wins[wi].LongCompleted++
		}
		if r.category != "" {
			errs++
			cats[r.category]++
			buckets[di].Errors++
			wins[wi].Failed++
			switch r.category {
			case "http_5xx":
				wins[wi].Err5xx++
			case "http_4xx":
				wins[wi].Err4xx++
			case "timeout":
				wins[wi].ErrTimeout++
			default:
				wins[wi].ErrTransport++
			}
		} else {
			okLat = append(okLat, r.lat*1000)
			lats[di] = append(lats[di], r.lat*1000)
			wins[wi].OK++
			wins[wi].LatSumOKMs += r.lat * 1000
			wins[wi].HistOK[strconv.Itoa(histIndex(r.lat*1000))]++
		}
		if r.sample {
			samples = append(samples, r)
		}
	}
	for i := range buckets {
		sort.Float64s(lats[i])
		buckets[i].P50Ms, buckets[i].P95Ms, buckets[i].P99Ms = pct(lats[i], .5), pct(lats[i], .95), pct(lats[i], .99)
	}
	sort.Float64s(okLat)
	sort.Float64s(allLat)
	sort.Float64s(lags)
	sort.Slice(samples, func(i, j int) bool { return samples[i].sched < samples[j].sched })
	completed := len(results)
	lagP99 := pct(lags, .99) * 1000
	errRate := 0.0
	if completed > 0 {
		errRate = float64(errs) / float64(completed)
	}
	var scenarioDoc any
	_ = json.Unmarshal(rawScenario, &scenarioDoc)

	summary := map[string]any{
		"duration_s": elapsed, "offered_total": float64(len(sched)), "sent_total": float64(len(results)),
		"completed_total": float64(completed), "errors_total": float64(errs), "error_rate": errRate,
		"errors_by_category":   cats,
		"dropped_by_generator": float64(dropped),
		"offered_rps_avg":      float64(len(sched)) / total,
		"completed_rps_avg":    float64(completed) / elapsed,
		"latency_p50_ms":       pct(allLat, .5), "latency_p95_ms": pct(allLat, .95), "latency_p99_ms": pct(allLat, .99),
		"latency_ok_p95_ms":   pct(okLat, .95),
		"schedule_lag_p50_ms": pct(lags, .5) * 1000, "schedule_lag_p99_ms": lagP99, "schedule_lag_max_ms": pct(lags, 1) * 1000,
		"generator_ok": dropped == 0 && lagP99 < 25,
	}
	if sc.Mix.Clients > 0 {
		total, modal, multi := 0, 0, 0
		for _, m := range affinity {
			best, sum := 0, 0
			for _, c := range m {
				sum += c
				if c > best {
					best = c
				}
			}
			total += sum
			modal += best
			if len(m) > 1 {
				multi++
			}
		}
		stick := 0.0
		if total > 0 {
			stick = float64(modal) / float64(total)
		}
		summary["affinity"] = map[string]any{"clients_seen": len(affinity), "requests_tracked": total,
			"stickiness": stick, "clients_served_by_multiple_backends": multi}
	}
	return map[string]any{
		"generator": map[string]any{
			"go": runtime.Version(), "num_cpu": runtime.NumCPU(), "gomaxprocs": runtime.GOMAXPROCS(0),
			"started_at": start.UTC().Format(time.RFC3339Nano), "start_unix": float64(start.UnixNano()) / 1e9,
			"time_scale": scale, "seed": sc.Seed, "arrivals": orDefault(sc.Arrivals, "poisson"), "target": target,
			"window_s": windowS, "sample_rate": sampleRate,
		},
		"hist":     map[string]any{"base_ms": histBaseMs, "ratio": histRatio, "buckets": histBuckets, "report": "upper bucket bound"},
		"scenario": scenarioDoc,
		"summary":  summary,
		"seconds":  buckets, "windows": wins, "lb": lbs, "faults_applied": faults,
	}, samples
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
