package desktopapp

import (
	"sort"
	"time"
)

type headlessTransfer struct {
	ID               string
	Kind             string
	Name             string
	Reference        string
	Digest           string
	State            string
	Completed        int64
	Total            *int64
	NetworkBytes     int64
	PlanningComplete bool
	Branch           string
}
type headlessArtifact struct {
	ID        string         `json:"artifact_id"`
	Kind      string         `json:"kind"`
	Name      string         `json:"name"`
	Reference string         `json:"reference"`
	Digest    *string        `json:"digest"`
	State     string         `json:"state"`
	Completed int64          `json:"completed_bytes"`
	Total     *int64         `json:"total_bytes"`
	Rate      *float64       `json:"rate_bytes_per_second"`
	ETA       *float64       `json:"eta_seconds"`
	Attempt   int            `json:"attempt"`
	Error     *headlessError `json:"error"`
}
type headlessProgress struct {
	Updated   time.Time          `json:"updated_at"`
	Completed int64              `json:"completed_bytes"`
	Total     *int64             `json:"total_bytes"`
	Rate      *float64           `json:"rate_bytes_per_second"`
	ETA       *float64           `json:"eta_seconds"`
	Planning  bool               `json:"planning_complete"`
	Artifacts []headlessArtifact `json:"artifacts"`
}
type headlessSample struct {
	at    time.Time
	bytes int64
}
type headlessArtifactTracker struct {
	value   headlessArtifact
	samples []headlessSample
	network int64
	started time.Time
	branch  string
}
type headlessProgressTracker struct {
	artifacts map[string]*headlessArtifactTracker
	planned   map[string]bool
}

func newHeadlessProgressTracker() *headlessProgressTracker {
	return &headlessProgressTracker{artifacts: map[string]*headlessArtifactTracker{}, planned: map[string]bool{"image": false, "kernel": false}}
}
func headlessPtr[T any](v T) *T { return &v }
func (p *headlessProgressTracker) update(t headlessTransfer, now time.Time) {
	if t.PlanningComplete {
		p.planned[t.Branch] = true
	}
	if t.ID == "" {
		return
	}
	a := p.artifacts[t.ID]
	if a == nil {
		a = &headlessArtifactTracker{value: headlessArtifact{ID: t.ID, Attempt: 1}, started: now, branch: t.Branch}
		p.artifacts[t.ID] = a
	}
	if t.Completed < a.value.Completed && t.State != "cached" {
		a.value.Attempt++
		a.samples = nil
		a.started = now
	}
	if delta := t.NetworkBytes - a.network; delta > 0 {
		a.samples = append(a.samples, headlessSample{now, delta})
	}
	a.network = t.NetworkBytes
	a.value.Kind = t.Kind
	a.value.Name = t.Name
	a.value.Reference = t.Reference
	a.value.State = t.State
	a.value.Completed = t.Completed
	a.value.Total = t.Total
	if t.Digest != "" {
		a.value.Digest = headlessPtr(t.Digest)
	}
}
func (p *headlessProgressTracker) finish(err error, now time.Time) {
	if err == nil {
		p.planned["image"] = true
		p.planned["kernel"] = true
	}
	for _, a := range p.artifacts {
		if a.value.State == "cached" || a.value.State == "ready" {
			continue
		}
		if err == nil {
			a.value.State = "ready"
		} else {
			a.value.State = "failed"
			a.value.Error = asHeadlessError(err, "image_pull_failed")
			if branch, ok := a.value.Error.Details["branch"].(string); ok && branch != a.branch {
				a.value.State = "cancelled"
				a.value.Error = headlessFailure("operation_cancelled", "Cancelled after another preparation branch failed")
			}
		}
	}
}
func (p *headlessProgressTracker) snapshot(now time.Time) *headlessProgress {
	result := &headlessProgress{Updated: now.UTC(), Planning: p.planned["image"] && p.planned["kernel"], Artifacts: []headlessArtifact{}}
	known := result.Planning
	var total int64
	var rate float64
	allDownloaded := result.Planning
	for _, a := range p.artifacts {
		cutoff := now.Add(-5 * time.Second)
		first := 0
		for first < len(a.samples) && a.samples[first].at.Before(cutoff) {
			first++
		}
		a.samples = append(a.samples[:0], a.samples[first:]...)
		v := a.value
		v.Rate = nil
		v.ETA = nil
		terminal := v.State == "cached" || v.State == "ready" || v.State == "verifying" || v.State == "preparing"
		if terminal {
			v.Rate = headlessPtr(0.0)
			v.ETA = headlessPtr(0.0)
		} else if v.State == "downloading" || v.State == "retrying" {
			duration := min(5.0, now.Sub(a.started).Seconds())
			var bytes int64
			for _, sample := range a.samples {
				bytes += sample.bytes
			}
			if duration > 0 {
				v.Rate = headlessPtr(float64(bytes) / duration)
			}
			if v.Total != nil && v.Rate != nil && *v.Rate > 0 {
				v.ETA = headlessPtr(float64(max(0, *v.Total-v.Completed)) / *v.Rate)
			}
		}
		if v.State == "cached" {
			v.Completed = 0
			v.Total = headlessPtr(int64(0))
		}
		result.Completed += v.Completed
		if v.Total == nil {
			known = false
			allDownloaded = false
		} else {
			total += *v.Total
			if v.Completed < *v.Total {
				allDownloaded = false
			}
		}
		if !terminal {
			allDownloaded = false
		}
		if v.Rate != nil {
			rate += *v.Rate
		}
		result.Artifacts = append(result.Artifacts, v)
	}
	sort.Slice(result.Artifacts, func(i, j int) bool { return result.Artifacts[i].ID < result.Artifacts[j].ID })
	if known {
		result.Total = &total
	}
	result.Rate = &rate
	if allDownloaded {
		result.ETA = headlessPtr(0.0)
	} else if known && rate > 0 {
		result.ETA = headlessPtr(float64(max(0, total-result.Completed)) / rate)
	}
	return result
}
func (p *headlessProgressTracker) phase(state string) string {
	if state == "succeeded" || state == "failed" || state == "cancelled" {
		return "complete"
	}
	for _, a := range p.artifacts {
		if a.value.State == "downloading" {
			return "downloading"
		}
	}
	if p.planned["image"] && p.planned["kernel"] {
		return "preparing"
	}
	return "resolving"
}
