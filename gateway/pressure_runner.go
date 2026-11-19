// Science runners' host metrics for the network pressure statistics (pressure.go): what limits the
// runners that check the science (CPU, memory, disk, the Lean build slots), next to what limits the
// GPUs. A small collector on each runner (the ops repo's runner bootstrap, a systemd timer) posts
// one report a minute to POST /v1/projects/<id>/runner-metrics with that project's key; the gateway
// checks it against a strict schema and adds it to the hour's and the day's buckets.
//
// Aggregates only. A report carries numbers and one opaque runner id, a random value the collector
// draws at boot (never an instance id, a host name or an address: the id must be 16-32 lowercase
// hex characters, which an EC2 instance id is not). The id lives in memory only, to count the
// runners and to accept at most one report a minute from each; it is never saved or served. A
// project that keeps its queue private (no public_queue) is not counted: its runners would show
// that it has work.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"regexp"
	"time"
)

const (
	runnerSchema       = 1
	maxRunnerBody      = 16 << 10
	runnerMinInterval  = 20 * time.Second // per runner; the collector posts every minute
	runnerFresh        = 150 * time.Second
	runnerForget       = 15 * time.Minute // a runner not heard from this long is forgotten
	runnerMaxTracked   = 500              // runners kept in memory at once; more are refused
	runnerMaxSkew      = 10 * time.Minute
	runnerMaxDurations = 300 // Lean runs or waits in one report
)

// runnerIDRe: a random id drawn at the runner's boot. Lowercase hex only, so an instance id
// ("i-0abc..."), a host name or an address cannot be sent as one.
var runnerIDRe = regexp.MustCompile(`^[0-9a-f]{16,32}$`)

// runnerReport is one runner's last minute (schema 1). Every field is listed: others are refused.
// Shares are 0-1 of the period, memory in GB, rates per second.
type runnerReport struct {
	Schema           int        `json:"schema"`
	Runner           string     `json:"runner"`
	At               time.Time  `json:"at"`
	PeriodS          float64    `json:"period_s"`
	CPUs             int        `json:"cpus"`
	CPUBusy          float64    `json:"cpu_busy"`   // all CPUs: not idle, not waiting for the disk
	CPUIOWait        float64    `json:"cpu_iowait"` // idle with a disk read or write outstanding
	CPUSteal         float64    `json:"cpu_steal"`  // taken by the hypervisor
	MemTotalGB       float64    `json:"mem_total_gb"`
	MemPrivateGB     float64    `json:"mem_private_gb"`      // anonymous memory (processes' own), average
	MemPrivatePeakGB float64    `json:"mem_private_peak_gb"` // and its peak
	PageCacheGB      float64    `json:"page_cache_gb"`       // file pages in memory (Lean's library among them), average
	PageCacheLowGB   float64    `json:"page_cache_low_gb"`   // and its low
	RefaultsS        float64    `json:"refaults_s"`          // evicted file pages read again (the page cache too small)
	PSI              *runnerPSI `json:"psi,omitempty"`       // pressure stall information, where the kernel has it
	DiskReadMBs      float64    `json:"disk_read_mb_s"`
	DiskWriteMBs     float64    `json:"disk_write_mb_s"`
	FreeGB           float64    `json:"free_gb"` // the work volume's free space, at the end of the period
	LeanSlots        int        `json:"lean_slots"`
	LeanBusy         float64    `json:"lean_busy"` // slots held, average
	LeanBusyPeak     int        `json:"lean_busy_peak"`
	LeanWaiting      float64    `json:"lean_waiting"` // builds waiting for a slot, average
	LeanWaitingPeak  int        `json:"lean_waiting_peak"`
	LeanRunsS        []float64  `json:"lean_runs_s"`  // each Lean process that ended: its run time
	LeanWaitsS       []float64  `json:"lean_waits_s"` // each wait for a slot that ended
	Sessions         float64    `json:"sessions"`     // agent sessions running (sandbox containers), average
	SessionsPeak     int        `json:"sessions_peak"`
	SpotNotice       bool       `json:"spot_notice"` // the cloud announced this runner's interruption
}

type runnerPSI struct {
	CPU    float64 `json:"cpu"` // "some": share of the period at least one task stalled on it
	Memory float64 `json:"memory"`
	IO     float64 `json:"io"`
}

func (r *runnerReport) validate(now time.Time) error {
	if r.Schema != runnerSchema {
		return fmt.Errorf("schema: must be %d", runnerSchema)
	}
	if !runnerIDRe.MatchString(r.Runner) {
		return errors.New("runner: 16-32 lowercase hex characters, a random id drawn at boot (never an instance id)")
	}
	if r.At.IsZero() || r.At.Before(now.Add(-runnerMaxSkew)) || r.At.After(now.Add(runnerMaxSkew)) {
		return errors.New("at: missing, or more than 10 minutes off the gateway's clock")
	}
	if r.CPUs < 1 || r.CPUs > 1024 || r.LeanSlots < 0 || r.LeanSlots > 1024 || r.LeanBusyPeak < 0 || r.LeanBusyPeak > 1024 ||
		r.LeanWaitingPeak < 0 || r.LeanWaitingPeak > 10000 || r.SessionsPeak < 0 || r.SessionsPeak > 1024 {
		return errors.New("cpus 1-1024; lean_slots, lean_busy_peak, sessions_peak 0-1024; lean_waiting_peak 0-10000")
	}
	if len(r.LeanRunsS) > runnerMaxDurations || len(r.LeanWaitsS) > runnerMaxDurations {
		return fmt.Errorf("lean_runs_s, lean_waits_s: at most %d each", runnerMaxDurations)
	}
	errs := []error{
		checkNum("period_s", r.PeriodS, 600), checkNum("cpu_busy", r.CPUBusy, 1), checkNum("cpu_iowait", r.CPUIOWait, 1),
		checkNum("cpu_steal", r.CPUSteal, 1), checkNum("mem_total_gb", r.MemTotalGB, 1<<16),
		checkNum("mem_private_gb", r.MemPrivateGB, 1<<16), checkNum("mem_private_peak_gb", r.MemPrivatePeakGB, 1<<16),
		checkNum("page_cache_gb", r.PageCacheGB, 1<<16), checkNum("page_cache_low_gb", r.PageCacheLowGB, 1<<16),
		checkNum("refaults_s", r.RefaultsS, 1e8), checkNum("disk_read_mb_s", r.DiskReadMBs, 1e6),
		checkNum("disk_write_mb_s", r.DiskWriteMBs, 1e6), checkNum("free_gb", r.FreeGB, 1e6),
		checkNum("lean_busy", r.LeanBusy, 1024), checkNum("lean_waiting", r.LeanWaiting, 10000), checkNum("sessions", r.Sessions, 1024),
	}
	if r.PeriodS < 10 {
		errs = append(errs, errors.New("period_s: 10 to 600"))
	}
	if r.PSI != nil {
		errs = append(errs, checkNum("psi.cpu", r.PSI.CPU, 1), checkNum("psi.memory", r.PSI.Memory, 1), checkNum("psi.io", r.PSI.IO, 1))
	}
	for i, d := range append(append([]float64{}, r.LeanRunsS...), r.LeanWaitsS...) {
		errs = append(errs, checkNum(fmt.Sprintf("lean durations[%d]", i), d, 86400))
	}
	return errors.Join(errs...)
}

// fstat is a per-report number summed over the reports (their count gives the average), with its
// peak and its low.
type fstat struct {
	Sum  float64 `json:"sum"`
	Peak float64 `json:"peak"`
	Low  float64 `json:"low"`
}

// add counts v, with peak and low (v itself when the report gives no separate ones); n is the
// count of earlier reports, so the first sets the low.
func (f *fstat) add(v, peak, low float64, n int) {
	f.Sum += v
	f.Peak = math.Max(f.Peak, peak)
	if n == 0 || low < f.Low {
		f.Low = low
	}
}

func (f *fstat) merge(o fstat, n, on int) {
	f.Sum += o.Sum
	f.Peak = math.Max(f.Peak, o.Peak)
	if on > 0 && (n == 0 || o.Low < f.Low) {
		f.Low = o.Low
	}
}

// runnerAgg is the runners' reports in one bucket: each report is one runner-minute.
type runnerAgg struct {
	Reports     int     `json:"reports"`
	CPU         fstat   `json:"cpu"`
	IOWait      fstat   `json:"iowait"`
	Steal       fstat   `json:"steal"`
	MemTotal    fstat   `json:"mem_total_gb"`
	MemPrivate  fstat   `json:"mem_private_gb"`
	PageCache   fstat   `json:"page_cache_gb"`
	Refaults    fstat   `json:"refaults_s"`
	PSIReports  int     `json:"psi_reports,omitempty"`
	PSICPU      fstat   `json:"psi_cpu"`
	PSIMemory   fstat   `json:"psi_memory"`
	PSIIO       fstat   `json:"psi_io"`
	DiskRead    fstat   `json:"disk_read_mb_s"`
	DiskWrite   fstat   `json:"disk_write_mb_s"`
	Free        fstat   `json:"free_gb"`
	LeanSlots   fstat   `json:"lean_slots"`
	LeanBusy    fstat   `json:"lean_busy"`
	LeanWaiting fstat   `json:"lean_waiting"`
	LeanRun     durHist `json:"lean_run"`
	LeanWait    durHist `json:"lean_wait"`
	Sessions    fstat   `json:"sessions"`
	SpotNotices int     `json:"spot_notices,omitempty"` // runners the cloud announced it would interrupt
}

func (a *runnerAgg) add(r *runnerReport, spot bool) {
	n := a.Reports
	a.Reports++
	a.CPU.add(r.CPUBusy, r.CPUBusy, r.CPUBusy, n)
	a.IOWait.add(r.CPUIOWait, r.CPUIOWait, r.CPUIOWait, n)
	a.Steal.add(r.CPUSteal, r.CPUSteal, r.CPUSteal, n)
	a.MemTotal.add(r.MemTotalGB, r.MemTotalGB, r.MemTotalGB, n)
	a.MemPrivate.add(r.MemPrivateGB, math.Max(r.MemPrivatePeakGB, r.MemPrivateGB), r.MemPrivateGB, n)
	a.PageCache.add(r.PageCacheGB, r.PageCacheGB, math.Min(r.PageCacheLowGB, r.PageCacheGB), n)
	a.Refaults.add(r.RefaultsS, r.RefaultsS, r.RefaultsS, n)
	if r.PSI != nil {
		pn := a.PSIReports
		a.PSIReports++
		a.PSICPU.add(r.PSI.CPU, r.PSI.CPU, r.PSI.CPU, pn)
		a.PSIMemory.add(r.PSI.Memory, r.PSI.Memory, r.PSI.Memory, pn)
		a.PSIIO.add(r.PSI.IO, r.PSI.IO, r.PSI.IO, pn)
	}
	a.DiskRead.add(r.DiskReadMBs, r.DiskReadMBs, r.DiskReadMBs, n)
	a.DiskWrite.add(r.DiskWriteMBs, r.DiskWriteMBs, r.DiskWriteMBs, n)
	a.Free.add(r.FreeGB, r.FreeGB, r.FreeGB, n)
	a.LeanSlots.add(float64(r.LeanSlots), float64(r.LeanSlots), float64(r.LeanSlots), n)
	a.LeanBusy.add(r.LeanBusy, math.Max(float64(r.LeanBusyPeak), r.LeanBusy), r.LeanBusy, n)
	a.LeanWaiting.add(r.LeanWaiting, math.Max(float64(r.LeanWaitingPeak), r.LeanWaiting), r.LeanWaiting, n)
	a.Sessions.add(r.Sessions, math.Max(float64(r.SessionsPeak), r.Sessions), r.Sessions, n)
	for _, s := range r.LeanRunsS {
		a.LeanRun.add(time.Duration(s * float64(time.Second)))
	}
	for _, s := range r.LeanWaitsS {
		a.LeanWait.add(time.Duration(s * float64(time.Second)))
	}
	if spot {
		a.SpotNotices++
	}
}

func (a *runnerAgg) merge(o *runnerAgg) {
	n, on := a.Reports, o.Reports
	for _, p := range []struct{ to, from *fstat }{{&a.CPU, &o.CPU}, {&a.IOWait, &o.IOWait}, {&a.Steal, &o.Steal},
		{&a.MemTotal, &o.MemTotal}, {&a.MemPrivate, &o.MemPrivate}, {&a.PageCache, &o.PageCache}, {&a.Refaults, &o.Refaults},
		{&a.DiskRead, &o.DiskRead}, {&a.DiskWrite, &o.DiskWrite}, {&a.Free, &o.Free}, {&a.LeanSlots, &o.LeanSlots},
		{&a.LeanBusy, &o.LeanBusy}, {&a.LeanWaiting, &o.LeanWaiting}, {&a.Sessions, &o.Sessions}} {
		p.to.merge(*p.from, n, on)
	}
	pn, pon := a.PSIReports, o.PSIReports
	for _, p := range []struct{ to, from *fstat }{{&a.PSICPU, &o.PSICPU}, {&a.PSIMemory, &o.PSIMemory}, {&a.PSIIO, &o.PSIIO}} {
		p.to.merge(*p.from, pn, pon)
	}
	a.Reports += on
	a.PSIReports += pon
	a.LeanRun.merge(&o.LeanRun)
	a.LeanWait.merge(&o.LeanWait)
	a.SpotNotices += o.SpotNotices
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }

func avgPeak(f fstat, n int) map[string]any {
	return map[string]any{"avg": round2(f.Sum / float64(max(n, 1))), "peak": round2(f.Peak)}
}

func durStat(h *durHist) map[string]any {
	return map[string]any{"count": h.count(), "p50": h.quantile(0.5), "p95": h.quantile(0.95)}
}

// public is the runners' part of a bucket as the endpoint serves it: averages over the
// runner-minutes, peaks, lows; no runner, instance or project.
func (a *runnerAgg) public() map[string]any {
	n := a.Reports
	m := map[string]any{
		"reports":         n,
		"cpu":             avgPeak(a.CPU, n),
		"iowait":          avgPeak(a.IOWait, n),
		"steal":           avgPeak(a.Steal, n),
		"mem_total_gb":    round2(a.MemTotal.Sum / float64(n)),
		"mem_private_gb":  avgPeak(a.MemPrivate, n),
		"page_cache_gb":   map[string]any{"avg": round2(a.PageCache.Sum / float64(n)), "low": round2(a.PageCache.Low)},
		"refaults_s":      avgPeak(a.Refaults, n),
		"disk_read_mb_s":  avgPeak(a.DiskRead, n),
		"disk_write_mb_s": avgPeak(a.DiskWrite, n),
		"free_gb_low":     round2(a.Free.Low),
		"lean": map[string]any{
			"slots":   round2(a.LeanSlots.Sum / float64(n)),
			"busy":    avgPeak(a.LeanBusy, n),
			"waiting": avgPeak(a.LeanWaiting, n),
			"run_s":   durStat(&a.LeanRun),
			"wait_s":  durStat(&a.LeanWait),
		},
		"sessions":     avgPeak(a.Sessions, n),
		"spot_notices": a.SpotNotices,
	}
	if a.PSIReports > 0 {
		m["psi"] = map[string]any{"cpu": avgPeak(a.PSICPU, a.PSIReports), "memory": avgPeak(a.PSIMemory, a.PSIReports),
			"io": avgPeak(a.PSIIO, a.PSIReports)}
	}
	return m
}

// runnerSeen is what the gateway remembers of a runner, in memory only.
type runnerSeen struct {
	last    time.Time
	spotted bool // its spot notice is counted
}

// runnerKey: one runner of one project. Never saved, never served.
type runnerKey struct{ project, runner string }

// runner counts one report in the hour and the day; false when the same runner reported less than
// runnerMinInterval ago, or too many runners are tracked.
func (p *pressure) runner(project string, r *runnerReport, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.runners == nil {
		p.runners = map[runnerKey]*runnerSeen{}
	}
	for k, s := range p.runners {
		if now.Sub(s.last) > runnerForget {
			delete(p.runners, k)
		}
	}
	k := runnerKey{project, r.Runner}
	s := p.runners[k]
	if s == nil {
		if len(p.runners) >= runnerMaxTracked {
			return false
		}
		s = &runnerSeen{}
		p.runners[k] = s
	} else if now.Sub(s.last) < runnerMinInterval && !now.Before(s.last) {
		return false
	}
	s.last = now
	spot := r.SpotNotice && !s.spotted
	s.spotted = s.spotted || r.SpotNotice
	h, d := p.bucketsLocked(now)
	for _, b := range []*pressBucket{h, d} {
		if b.Runner == nil {
			b.Runner = &runnerAgg{}
		}
		b.Runner.add(r, spot)
	}
	return true
}

// runnersFresh is the number of runners that reported within runnerFresh.
func (p *pressure) runnersFresh(now time.Time) int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, s := range p.runners {
		if now.Sub(s.last) <= runnerFresh {
			n++
		}
	}
	return n
}

// decodeRunnerReport reads one report: unknown fields and trailing data are refused.
func decodeRunnerReport(body []byte, now time.Time) (*runnerReport, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var r runnerReport
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("not a runner report: %w", err)
	}
	if dec.More() {
		return nil, errors.New("not a runner report: data after the object")
	}
	if err := r.validate(now); err != nil {
		return nil, err
	}
	return &r, nil
}

// handleRunnerMetrics serves POST /v1/projects/{id}/runner-metrics: the project's own key only.
// A project without public_queue gets 200 with counted false: its runners keep working, uncounted.
func (g *Gateway) handleRunnerMetrics(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !g.projectFor(w, r, id) {
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		apiError(w, http.StatusUnsupportedMediaType, "send the report as application/json")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRunnerBody))
	if err != nil {
		apiError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("a report is at most %d bytes", maxRunnerBody))
		return
	}
	now := time.Now()
	rep, err := decodeRunnerReport(body, now)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if p := g.project(bearer(r)); p == nil || !p.PublicQueue {
		writeJSON(w, http.StatusOK, map[string]any{"counted": false, "why": "the project does not publish its queue"})
		return
	}
	if !g.press.runner(id, rep, now) {
		w.Header().Set("Retry-After", "20")
		apiError(w, http.StatusTooManyRequests, "one report a minute per runner")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"counted": true})
}
