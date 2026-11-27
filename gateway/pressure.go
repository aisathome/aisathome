// Network pressure for the network page: how full the network was, per UTC hour (the last two
// weeks) and per UTC day (the last 90 days). What limits it now, GPU slots, the queue or the science
// runners (their CPU, memory, disk and Lean slots: pressure_runner.go), how the nodes spend their
// time (prefill or decode, prompt-cache hits: pressure_node.go), and the demand signal a runner pool
// can be sized by: demand = min(open science tasks, the GPU slots online that science may use).
//
// Public numbers come only from projects that publish their queue (public_queue): a project that
// keeps its queue private is in no count here, so its queue depth cannot be read off by subtraction.
//
// Gauges (slots, busy slots, requests waiting, held nodes, science slots, open tasks, demand) are
// sampled every pressureSampleEvery and kept as sum and peak; the peaks of the waiting requests
// also count every request that starts to wait, so a short burst between two samples is not lost.
// Durations (the wait for a slot, the time to the first token) go into log-spaced histograms per
// workload class, so that percentiles of an hour, a day or the last 24 hours come from the same
// counts. Each request counts once, by how it ended.
//
// Kept in state_dir/pressure.json, written every minute and at shutdown (a crash loses at most a
// minute): a file of its own, since the history file is written every few seconds and a pressure
// bucket is larger than a history day. Aggregates only: never a user, key, node, operator,
// project, runner, instance or prompt.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
)

const (
	pressureHours       = 14 * 24 // hourly buckets kept
	pressureDays        = 90      // daily buckets kept
	pressureSampleEvery = 10 * time.Second
	pressureSaveEvery   = time.Minute
	pressureCacheFor    = 15 * time.Second // a built answer is served again for this long
	pressureFile        = "pressure.json"
)

// Request outcomes in the pressure statistics: one per request, however many nodes it tried.
const (
	pressOK        = "ok"        // the answer reached the client
	pressFailed    = "failed"    // a node or the gateway failed it
	pressBlocked   = "blocked"   // a guard stopped the answer
	pressRefused   = "refused"   // no node took it: all busy until its time ran out, the queue full, none with the model or context
	pressPreempted = "preempted" // science stopped for a person's request and not placed again
	pressCancelled = "cancelled" // the client left
)

// durBins are the upper edges, in milliseconds, of a duration histogram's bins, log-spaced from
// 10 ms (no wait) to an hour; one more bin takes everything longer.
var durBins = []int64{10, 50, 100, 200, 500, 1000, 2000, 3000, 5000, 7500, 10_000, 15_000, 20_000, 30_000, 45_000,
	60_000, 90_000, 120_000, 180_000, 240_000, 300_000, 450_000, 600_000, 900_000, 1_200_000, 1_800_000, 3_600_000}

// durHist counts durations by bin (durBins), with the longest one seen.
type durHist struct {
	C   []int64 `json:"c"` // per bin; empty bins at the end are left out
	Max int64   `json:"max_ms"`
}

func (h *durHist) add(d time.Duration) {
	ms := max(d.Milliseconds(), 0)
	i := sort.Search(len(durBins), func(i int) bool { return durBins[i] >= ms })
	for len(h.C) <= i {
		h.C = append(h.C, 0)
	}
	h.C[i]++
	h.Max = max(h.Max, ms)
}

func (h *durHist) merge(o *durHist) {
	for len(h.C) < len(o.C) {
		h.C = append(h.C, 0)
	}
	for i, c := range o.C {
		h.C[i] += max(c, 0)
	}
	h.Max = max(h.Max, o.Max)
}

func (h *durHist) count() int64 {
	n := int64(0)
	for _, c := range h.C {
		n += max(c, 0)
	}
	return n
}

// quantile is the q-quantile in seconds: the upper edge of the bin it falls in, at most the
// longest duration seen, and 0 in the first bin (no wait to speak of). It errs long, never short.
func (h *durHist) quantile(q float64) float64 {
	n := h.count()
	if n == 0 {
		return 0
	}
	rank := max(int64(math.Ceil(q*float64(n))), 1)
	seen := int64(0)
	for i, c := range h.C {
		if seen += max(c, 0); seen < rank {
			continue
		}
		if i == 0 {
			return 0
		}
		edge := h.Max
		if i < len(durBins) {
			edge = min(durBins[i], h.Max)
		}
		return round1(float64(edge) / 1000)
	}
	return round1(float64(h.Max) / 1000)
}

// gauge is a sampled number: the sum of its samples (the bucket's samples give the average) and
// its peak.
type gauge struct {
	Sum  int64 `json:"sum"`
	Peak int   `json:"peak"`
}

func (g *gauge) add(v int) {
	g.Sum += int64(max(v, 0))
	g.Peak = max(g.Peak, v)
}

// pressSample is the network at one moment.
type pressSample struct {
	Slots        int `json:"slots"`         // GPU slots on nodes online, not held, not paused
	Busy         int `json:"busy"`          // of those, slots with a request
	QueuePeople  int `json:"queue_people"`  // chat and agent requests waiting for a slot
	QueueScience int `json:"queue_science"` // science requests waiting for a slot
	Held         int `json:"held"`          // nodes held by a guard
	ScienceSlots int `json:"science_slots"` // slots science may use (agents, its share of nodes)
	OpenTasks    int `json:"open_tasks"`    // science tasks queued or running
	Demand       int `json:"demand"`        // min(open tasks, science slots), per project, capped by the slots of the projects with work
	Runners      int `json:"runners"`       // science runners that reported in the last runnerFresh
}

// pressBucket is one hour or one day.
type pressBucket struct {
	Start        string              `json:"start"`
	Samples      int                 `json:"samples"`
	Slots        gauge               `json:"slots"`
	Busy         gauge               `json:"busy"`
	QueuePeople  gauge               `json:"queue_people"`
	QueueScience gauge               `json:"queue_science"`
	Held         gauge               `json:"held"`
	ScienceSlots gauge               `json:"science_slots"`
	OpenTasks    gauge               `json:"open_tasks"`
	Demand       gauge               `json:"demand"`
	QueueAll     gauge               `json:"queue_all"` // people's and science requests waiting together
	Runners      gauge               `json:"runners"`
	Wait         map[string]*durHist `json:"wait,omitempty"`        // by class: arrival to a slot (or to giving up)
	FirstToken   map[string]*durHist `json:"first_token,omitempty"` // by class: arrival to the first generated token
	Outcomes     map[string]int      `json:"outcomes,omitempty"`
	Holds        int                 `json:"holds,omitempty"`  // nodes a guard held
	Runner       *runnerAgg          `json:"runner,omitempty"` // the science runners' reports (pressure_runner.go)
	Node         *nodeAgg            `json:"node,omitempty"`   // how the nodes spent their time (pressure_node.go)
}

func (b *pressBucket) sample(s pressSample) {
	b.Samples++
	b.Slots.add(s.Slots)
	b.Busy.add(s.Busy)
	b.QueuePeople.add(s.QueuePeople)
	b.QueueScience.add(s.QueueScience)
	b.Held.add(s.Held)
	b.ScienceSlots.add(s.ScienceSlots)
	b.OpenTasks.add(s.OpenTasks)
	b.Demand.add(s.Demand)
	b.QueueAll.add(s.QueuePeople + s.QueueScience)
	b.Runners.add(s.Runners)
}

func histIn(m *map[string]*durHist, k string) *durHist {
	if *m == nil {
		*m = map[string]*durHist{}
	}
	h := (*m)[k]
	if h == nil {
		h = &durHist{}
		(*m)[k] = h
	}
	return h
}

// merge adds o to b (for the sum of a range).
func (b *pressBucket) merge(o *pressBucket) {
	b.Samples += o.Samples
	for _, p := range []struct{ to, from *gauge }{{&b.Slots, &o.Slots}, {&b.Busy, &o.Busy}, {&b.QueuePeople, &o.QueuePeople},
		{&b.QueueScience, &o.QueueScience}, {&b.Held, &o.Held}, {&b.ScienceSlots, &o.ScienceSlots},
		{&b.OpenTasks, &o.OpenTasks}, {&b.Demand, &o.Demand}, {&b.QueueAll, &o.QueueAll}, {&b.Runners, &o.Runners}} {
		p.to.Sum += p.from.Sum
		p.to.Peak = max(p.to.Peak, p.from.Peak)
	}
	for k, h := range o.Wait {
		histIn(&b.Wait, k).merge(h)
	}
	for k, h := range o.FirstToken {
		histIn(&b.FirstToken, k).merge(h)
	}
	for k, n := range o.Outcomes {
		if b.Outcomes == nil {
			b.Outcomes = map[string]int{}
		}
		b.Outcomes[k] += n
	}
	b.Holds += o.Holds
	if o.Runner != nil {
		if b.Runner == nil {
			b.Runner = &runnerAgg{}
		}
		b.Runner.merge(o.Runner)
	}
	if o.Node != nil {
		if b.Node == nil {
			b.Node = &nodeAgg{}
		}
		b.Node.merge(o.Node)
	}
}

type pressure struct {
	mu     sync.Mutex
	hours  map[string]*pressBucket
	days   map[string]*pressBucket
	path   string // state file; "" = memory only
	dirty  bool
	saveMu sync.Mutex

	cacheMu sync.Mutex
	cache   map[string]pressCached // built answers, by range

	runners map[runnerKey]*runnerSeen // runners heard from lately (pressure_runner.go); memory only
}

type pressCached struct {
	at   time.Time
	body []byte
}

func newPressure() *pressure {
	return &pressure{hours: map[string]*pressBucket{}, days: map[string]*pressBucket{}, cache: map[string]pressCached{}}
}

// bucketsLocked returns the hour and the day of now, creating them and dropping the buckets that
// fell out of their window. Caller holds p.mu.
func (p *pressure) bucketsLocked(now time.Time) (hour, day *pressBucket) {
	now = now.UTC()
	hk, dk := now.Format(hourFormat), now.Format(time.DateOnly)
	if hour = p.hours[hk]; hour == nil {
		hour = &pressBucket{Start: hk}
		p.hours[hk] = hour
		p.pruneLocked(now)
	}
	if day = p.days[dk]; day == nil {
		day = &pressBucket{Start: dk}
		p.days[dk] = day
		p.pruneLocked(now)
	}
	p.dirty = true
	return hour, day
}

// pruneLocked drops the hours before the last pressureHours and the days before the last
// pressureDays, counted back from now. Caller holds p.mu.
func (p *pressure) pruneLocked(now time.Time) {
	now = now.UTC()
	firstHour := now.Truncate(time.Hour).Add(-(pressureHours - 1) * time.Hour).Format(hourFormat)
	for k := range p.hours {
		if k < firstHour {
			delete(p.hours, k)
		}
	}
	firstDay := now.AddDate(0, 0, -(pressureDays - 1)).Format(time.DateOnly)
	for k := range p.days {
		if k < firstDay {
			delete(p.days, k)
		}
	}
}

// sample counts the network as it is at now.
func (p *pressure) sample(s pressSample, now time.Time) {
	if p == nil { // a gateway built without New (tests)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	h, d := p.bucketsLocked(now)
	h.sample(s)
	d.sample(s)
}

// queuePeak notes the requests waiting now, when one starts to wait: the peaks see bursts that
// end between two samples.
func (p *pressure) queuePeak(people, science int, now time.Time) {
	if p == nil { // a gateway built without New (tests)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	h, d := p.bucketsLocked(now)
	for _, b := range []*pressBucket{h, d} {
		b.QueuePeople.Peak = max(b.QueuePeople.Peak, people)
		b.QueueScience.Peak = max(b.QueueScience.Peak, science)
		b.QueueAll.Peak = max(b.QueueAll.Peak, people+science)
	}
}

// wait counts how long a request of class waited for a slot.
func (p *pressure) wait(class string, d time.Duration, now time.Time) {
	if p == nil { // a gateway built without New (tests)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	h, day := p.bucketsLocked(now)
	histIn(&h.Wait, class).add(d)
	histIn(&day.Wait, class).add(d)
}

// firstToken counts how long a request of class took from its arrival to its first generated token.
func (p *pressure) firstToken(class string, d time.Duration, now time.Time) {
	if p == nil { // a gateway built without New (tests)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	h, day := p.bucketsLocked(now)
	histIn(&h.FirstToken, class).add(d)
	histIn(&day.FirstToken, class).add(d)
}

// outcome counts how a request ended.
func (p *pressure) outcome(o string, now time.Time) {
	if p == nil { // a gateway built without New (tests)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	h, d := p.bucketsLocked(now)
	for _, b := range []*pressBucket{h, d} {
		if b.Outcomes == nil {
			b.Outcomes = map[string]int{}
		}
		b.Outcomes[o]++
	}
}

// hold counts a node a guard held.
func (p *pressure) hold(now time.Time) {
	if p == nil { // a gateway built without New (tests)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	h, d := p.bucketsLocked(now)
	h.Holds++
	d.Holds++
}

type pressureState struct {
	Version int            `json:"version"`
	Hours   []*pressBucket `json:"hours"`
	Days    []*pressBucket `json:"days"`
}

// pressureVersion is the state file's format: 2 since private projects are left out of every count.
const pressureVersion = 2

// load reads the state file. A missing file is a first start; a corrupt one is logged and set
// aside, as the history's is.
func (p *pressure) load(log *slog.Logger, now time.Time) {
	raw, err := os.ReadFile(p.path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var st pressureState
	if err == nil {
		err = json.Unmarshal(raw, &st)
	}
	if err != nil {
		log.Warn("pressure statistics unreadable, setting them aside", "file", p.path, "err", err)
		os.Rename(p.path, p.path+".bad")
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, b := range append(st.Hours, st.Days...) {
		if b != nil && st.Version < pressureVersion {
			// Written before private projects were left out (version 1): their open tasks, demand and
			// node timings may be in these numbers, which are public. Dropped, not republished.
			b.OpenTasks, b.Demand, b.Node = gauge{}, gauge{}, nil
		}
	}
	for _, b := range st.Hours {
		if b != nil && b.Start != "" {
			p.hours[b.Start] = b
		}
	}
	for _, b := range st.Days {
		if b != nil && b.Start != "" {
			p.days[b.Start] = b
		}
	}
	p.pruneLocked(now)
}

func sortedBuckets(m map[string]*pressBucket) []*pressBucket {
	out := make([]*pressBucket, 0, len(m))
	for _, b := range m {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

// save writes the state file if anything changed since the last save.
func (p *pressure) save() error {
	p.saveMu.Lock()
	defer p.saveMu.Unlock()
	p.mu.Lock()
	if p.path == "" || !p.dirty {
		p.mu.Unlock()
		return nil
	}
	raw, err := json.Marshal(pressureState{Version: pressureVersion, Hours: sortedBuckets(p.hours), Days: sortedBuckets(p.days)})
	p.dirty = false
	p.mu.Unlock()
	if err == nil {
		err = writeFileAtomic(p.path, raw)
	}
	if err != nil {
		p.mu.Lock()
		p.dirty = true
		p.mu.Unlock()
	}
	return err
}

// pressRanges are the ranges GET /v1/stats/pressure serves: hourly buckets up to two weeks,
// daily ones beyond.
var pressRanges = map[string]struct {
	hourly bool
	n      int
}{"24h": {true, 24}, "48h": {true, 48}, "7d": {true, 7 * 24}, "14d": {true, pressureHours},
	"30d": {false, 30}, "90d": {false, pressureDays}}

// stat is a gauge in the public answer: average over the samples, and peak.
func stat(g gauge, samples int) map[string]any {
	avg := 0.0
	if samples > 0 {
		avg = math.Round(float64(g.Sum)/float64(samples)*100) / 100
	}
	return map[string]any{"avg": avg, "peak": g.Peak}
}

func durStats(m map[string]*durHist) map[string]any {
	out := map[string]any{}
	for _, c := range classes {
		h := m[c]
		if h == nil || h.count() == 0 {
			continue
		}
		out[c] = map[string]any{"count": h.count(), "p50": h.quantile(0.5), "p95": h.quantile(0.95)}
	}
	return out
}

// public is a bucket (or a range's sum) as the endpoint serves it.
func (b *pressBucket) public() map[string]any {
	m := map[string]any{
		"samples":       b.Samples,
		"slots":         stat(b.Slots, b.Samples),
		"busy":          stat(b.Busy, b.Samples),
		"queue":         map[string]any{"people": stat(b.QueuePeople, b.Samples), "science": stat(b.QueueScience, b.Samples), "all": stat(b.QueueAll, b.Samples)},
		"held":          stat(b.Held, b.Samples),
		"holds":         b.Holds,
		"science_slots": stat(b.ScienceSlots, b.Samples),
		"open_tasks":    stat(b.OpenTasks, b.Samples),
		"demand":        stat(b.Demand, b.Samples),
		"runners":       stat(b.Runners, b.Samples),
		"wait_s":        durStats(b.Wait),
		"first_token_s": durStats(b.FirstToken),
		"outcomes":      map[string]int{},
	}
	if b.Start != "" {
		m["start"] = b.Start
	}
	if b.Slots.Sum > 0 { // busy slot-time over slot-time online
		m["utilisation"] = math.Round(float64(min(b.Busy.Sum, b.Slots.Sum))/float64(b.Slots.Sum)*1000) / 1000
	}
	for _, o := range []string{pressOK, pressFailed, pressBlocked, pressRefused, pressPreempted, pressCancelled} {
		if n := b.Outcomes[o]; n > 0 {
			m["outcomes"].(map[string]int)[o] = n
		}
	}
	if b.Runner != nil && b.Runner.Reports > 0 {
		m["runner"] = b.Runner.public()
	}
	if b.Node != nil && b.Node.Requests > 0 {
		m["node"] = b.Node.public()
	}
	return m
}

// view is the answer for a range: its buckets, oldest first (hours or days with no data are left
// out), their sum, and the sum of the last 24 hours.
func (p *pressure) view(rng string, now time.Time) map[string]any {
	r := pressRanges[rng]
	now = now.UTC()
	p.mu.Lock()
	defer p.mu.Unlock()
	var first string
	src := p.days
	if r.hourly {
		first, src = now.Truncate(time.Hour).Add(-time.Duration(r.n-1)*time.Hour).Format(hourFormat), p.hours
	} else {
		first = now.AddDate(0, 0, -(r.n - 1)).Format(time.DateOnly)
	}
	buckets := []map[string]any{}
	total := &pressBucket{}
	for _, b := range sortedBuckets(src) {
		if b.Start >= first {
			buckets = append(buckets, b.public())
			total.merge(b)
		}
	}
	day := &pressBucket{}
	since := now.Truncate(time.Hour).Add(-23 * time.Hour).Format(hourFormat)
	for k, b := range p.hours {
		if k >= since {
			day.merge(b)
		}
	}
	unit := "day"
	if r.hourly {
		unit = "hour"
	}
	return map[string]any{"range": rng, "bucket": unit, "sample_seconds": int(pressureSampleEvery / time.Second),
		"buckets": buckets, "total": total.public(), "last_24h": day.public()}
}

// pressureNow samples the network: nodes and their slots, the queues, science's demand (of the
// projects that publish their queue) and the runners reporting.
func (g *Gateway) pressureNow() pressSample {
	var projs []*Project
	if pt := g.projects.Load(); pt != nil {
		for _, id := range pt.ids {
			if p := pt.byID[id]; p != nil && p.PublicQueue {
				projs = append(projs, p)
			}
		}
	}
	open := make([]int, len(projs))
	for i, p := range projs { // the task store's own lock, before g.mu
		if g.tasks != nil {
			open[i] = g.tasks.openCount(p.ID)
		}
	}
	ownOnly := g.accounts != nil && g.accounts.Settings().OwnOnly()
	s := pressSample{QueuePeople: int(g.queued.Load()), QueueScience: int(g.queuedSc.Load()), Runners: g.press.runnersFresh(time.Now())}
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	type up struct {
		n     *nodeConn
		slots int
	}
	var ups []up
	for _, n := range g.node {
		if n.held {
			s.Held++
			continue
		}
		n.mu.Lock()
		paused, busy := n.paused, n.inflight
		n.mu.Unlock()
		if paused {
			continue
		}
		sl := nodeSlots(n)
		s.Slots += sl
		s.Busy += min(max(busy, 0), sl)
		ups = append(ups, up{n, sl})
	}
	// Science: per project, the slots of the nodes its agent requests may use (with the context it asks
	// of a node), within its share of them (the largest nodes: an upper bound); its demand is what of
	// its open tasks those slots could run. Projects share nodes, so the total is capped by the slots of the nodes the projects with
	// work may use, each node once: a project without work adds no slot to that cap.
	all, withWork := map[*nodeConn]int{}, map[*nodeConn]int{}
	capSum, demand := 0, 0
	for i, p := range projs {
		rt := scienceRoute(p, ownOnly)
		var sl []int
		for _, u := range ups {
			if rt.allows(u.n, now) && rt.nodeClearsFloor(u.n) { // min_node_ctx: a smaller node never takes its sessions
				sl = append(sl, u.slots)
				all[u.n] = u.slots
				if open[i] > 0 {
					withWork[u.n] = u.slots
				}
			}
		}
		sort.Sort(sort.Reverse(sort.IntSlice(sl)))
		if k := shareCap(p.MaxNodeShare, len(sl)); k > 0 && k < len(sl) {
			sl = sl[:k]
		}
		c := 0
		for _, v := range sl {
			c += v
		}
		capSum += c
		demand += min(open[i], c)
		s.OpenTasks += open[i]
	}
	sum := func(m map[*nodeConn]int) int {
		t := 0
		for _, v := range m {
			t += v
		}
		return t
	}
	s.ScienceSlots = min(sum(all), capSum)
	s.Demand = min(demand, sum(withWork))
	return s
}

// nodeSlots is how many requests a node runs at once: its slots are shared by its models (pick),
// so the largest model's count, not their sum.
func nodeSlots(n *nodeConn) int {
	s := 0
	for _, m := range n.models {
		s = max(s, m.Slots)
	}
	return s
}

// openCount is the number of a project's tasks queued or running; its queue is read from the disk
// if no call loaded it yet, so a project with no worker running still shows its demand.
func (s *taskStore) openCount(project string) int {
	open, _ := s.scienceLoad(project) // the same count as connector demand's (tasks_store.go)
	return open
}

// handlePressure serves GET /v1/stats/pressure?range=24h|48h|7d|14d|30d|90d (default 7d) without
// authentication; answers are built at most every pressureCacheFor.
func (g *Gateway) handlePressure(w http.ResponseWriter, r *http.Request) {
	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = "7d"
	}
	if _, ok := pressRanges[rng]; !ok {
		apiError(w, http.StatusBadRequest, "range must be one of 24h, 48h, 7d, 14d (hourly), 30d or 90d (daily)")
		return
	}
	p := g.press
	now := time.Now()
	p.cacheMu.Lock()
	c, ok := p.cache[rng]
	if !ok || now.Sub(c.at) >= pressureCacheFor {
		v := p.view(rng, now)
		v["updated"] = now.UTC().Format(time.RFC3339)
		v["now"] = g.pressureNow()
		body, _ := json.Marshal(v)
		c = pressCached{at: now, body: append(body, '\n')}
		p.cache[rng] = c
	}
	p.cacheMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.Write(c.body)
}

// projectPublic reports whether requests of project id may count in public statistics: no project
// (people's requests), or a project that publishes its queue.
func (g *Gateway) projectPublic(id string) bool {
	if id == "" {
		return true
	}
	pt := g.projects.Load()
	if pt == nil {
		return false
	}
	p := pt.byID[id]
	return p != nil && p.PublicQueue
}
