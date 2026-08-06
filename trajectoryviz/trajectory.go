// Package trajectoryviz renders bounded, application-agnostic agent/job
// trajectories as a self-contained HTML animation.
package trajectoryviz

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	SchemaVersion  = 1
	maxAgents      = 2048
	maxPoints      = 2_000_000
	maxJobs        = 100_000
	maxAssignments = 1_000_000
	maxLineBytes   = 4 << 20
)

// Dataset is the normalized in-memory form accepted by RenderHTML.
// Agent and Job intentionally avoid domain words such as chair and ride.
type Dataset struct {
	Schema      int          `json:"schema"`
	Title       string       `json:"title,omitempty"`
	Agents      []Agent      `json:"agents,omitempty"`
	Jobs        []Job        `json:"jobs,omitempty"`
	Assignments []Assignment `json:"assignments,omitempty"`
}

type Agent struct {
	ID     string  `json:"id"`
	Label  string  `json:"label,omitempty"`
	Kind   string  `json:"kind,omitempty"`
	Points []Point `json:"points,omitempty"`
}

type Point struct {
	At time.Time `json:"at"`
	X  float64   `json:"x"`
	Y  float64   `json:"y"`
}

type Coordinate struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type Job struct {
	ID          string     `json:"id"`
	Label       string     `json:"label,omitempty"`
	RequestedAt time.Time  `json:"requested_at"`
	Pickup      Coordinate `json:"pickup"`
	Destination Coordinate `json:"destination"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
}

// Assignment changes the agent responsible for a job at At. An empty AgentID
// explicitly unassigns it. Multiple records for one job make lifelong or
// rolling-window re-optimization visible rather than assuming one immutable
// batch assignment.
type Assignment struct {
	JobID   string    `json:"job_id"`
	AgentID string    `json:"agent_id,omitempty"`
	At      time.Time `json:"at"`
}

type record struct {
	Type string `json:"type"`

	Schema int    `json:"schema,omitempty"`
	Title  string `json:"title,omitempty"`

	ID    string `json:"id,omitempty"`
	Label string `json:"label,omitempty"`
	Kind  string `json:"kind,omitempty"`

	AgentID string   `json:"agent_id,omitempty"`
	JobID   string   `json:"job_id,omitempty"`
	At      string   `json:"at,omitempty"`
	X       *float64 `json:"x,omitempty"`
	Y       *float64 `json:"y,omitempty"`

	RequestedAt string      `json:"requested_at,omitempty"`
	Pickup      *Coordinate `json:"pickup,omitempty"`
	Destination *Coordinate `json:"destination,omitempty"`
	FinishedAt  string      `json:"finished_at,omitempty"`
}

// ParseNDJSON reads the interchange format used by adapters. Records may be
// ordered arbitrarily; Normalize sorts time-series before validation.
func ParseNDJSON(r io.Reader) (Dataset, error) {
	ds := Dataset{Schema: SchemaVersion}
	agents := make(map[string]*Agent)
	declared := make(map[string]bool)

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), maxLineBytes)
	line := 0
	for scanner.Scan() {
		line++
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 || raw[0] == '#' {
			continue
		}
		var rec record
		if err := json.Unmarshal(raw, &rec); err != nil {
			return Dataset{}, fmt.Errorf("trajectoryviz: line %d: %w", line, err)
		}
		switch rec.Type {
		case "meta":
			if rec.Schema != 0 {
				ds.Schema = rec.Schema
			}
			if rec.Title != "" {
				ds.Title = rec.Title
			}
		case "agent":
			if rec.ID == "" {
				return Dataset{}, fmt.Errorf("trajectoryviz: line %d: agent id is required", line)
			}
			if declared[rec.ID] {
				return Dataset{}, fmt.Errorf("trajectoryviz: line %d: duplicate agent %q", line, rec.ID)
			}
			agent := agents[rec.ID]
			if agent == nil {
				agent = &Agent{ID: rec.ID}
				agents[rec.ID] = agent
			}
			agent.Label, agent.Kind = rec.Label, rec.Kind
			declared[rec.ID] = true
		case "point":
			if rec.AgentID == "" || rec.X == nil || rec.Y == nil {
				return Dataset{}, fmt.Errorf("trajectoryviz: line %d: point requires agent_id, x, and y", line)
			}
			at, err := parseTime(rec.At)
			if err != nil {
				return Dataset{}, fmt.Errorf("trajectoryviz: line %d: point at: %w", line, err)
			}
			agent := agents[rec.AgentID]
			if agent == nil {
				agent = &Agent{ID: rec.AgentID}
				agents[rec.AgentID] = agent
			}
			agent.Points = append(agent.Points, Point{At: at, X: *rec.X, Y: *rec.Y})
		case "job":
			if rec.ID == "" || rec.Pickup == nil || rec.Destination == nil {
				return Dataset{}, fmt.Errorf("trajectoryviz: line %d: job requires id, pickup, and destination", line)
			}
			requested, err := parseTime(rec.RequestedAt)
			if err != nil {
				return Dataset{}, fmt.Errorf("trajectoryviz: line %d: job requested_at: %w", line, err)
			}
			var finished *time.Time
			if rec.FinishedAt != "" {
				value, err := parseTime(rec.FinishedAt)
				if err != nil {
					return Dataset{}, fmt.Errorf("trajectoryviz: line %d: job finished_at: %w", line, err)
				}
				finished = &value
			}
			ds.Jobs = append(ds.Jobs, Job{ID: rec.ID, Label: rec.Label, RequestedAt: requested,
				Pickup: *rec.Pickup, Destination: *rec.Destination, FinishedAt: finished})
		case "assignment":
			if rec.JobID == "" {
				return Dataset{}, fmt.Errorf("trajectoryviz: line %d: assignment job_id is required", line)
			}
			at, err := parseTime(rec.At)
			if err != nil {
				return Dataset{}, fmt.Errorf("trajectoryviz: line %d: assignment at: %w", line, err)
			}
			ds.Assignments = append(ds.Assignments, Assignment{JobID: rec.JobID, AgentID: rec.AgentID, At: at})
		default:
			return Dataset{}, fmt.Errorf("trajectoryviz: line %d: unknown record type %q", line, rec.Type)
		}
	}
	if err := scanner.Err(); err != nil {
		return Dataset{}, fmt.Errorf("trajectoryviz: read input: %w", err)
	}

	// Points may arrive before their agent declaration. Rebuild the slice from
	// the map once, with stable IDs, so no pointer into an appendable slice is
	// retained.
	ds.Agents = ds.Agents[:0]
	for _, agent := range agents {
		ds.Agents = append(ds.Agents, *agent)
	}
	if err := ds.Normalize(); err != nil {
		return Dataset{}, err
	}
	return ds, nil
}

func parseTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, errors.New("timestamp is required")
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid RFC3339 timestamp %q", value)
	}
	return parsed, nil
}

// Normalize sorts all time-series and checks boundedness, references, finite
// coordinates, and timestamp order.
func (d *Dataset) Normalize() error {
	if d.Schema == 0 {
		d.Schema = SchemaVersion
	}
	if d.Schema != SchemaVersion {
		return fmt.Errorf("trajectoryviz: schema %d is unsupported (want %d)", d.Schema, SchemaVersion)
	}
	if len(d.Agents) > maxAgents || len(d.Jobs) > maxJobs || len(d.Assignments) > maxAssignments {
		return errors.New("trajectoryviz: dataset exceeds a bounded collection limit")
	}

	agents := make(map[string]struct{}, len(d.Agents))
	points := 0
	for i := range d.Agents {
		a := &d.Agents[i]
		a.ID = strings.TrimSpace(a.ID)
		if a.ID == "" {
			return errors.New("trajectoryviz: agent id is required")
		}
		if _, duplicate := agents[a.ID]; duplicate {
			return fmt.Errorf("trajectoryviz: duplicate agent %q", a.ID)
		}
		agents[a.ID] = struct{}{}
		points += len(a.Points)
		if points > maxPoints {
			return fmt.Errorf("trajectoryviz: point count exceeds %d", maxPoints)
		}
		for _, p := range a.Points {
			if p.At.IsZero() || !finite(p.X) || !finite(p.Y) {
				return fmt.Errorf("trajectoryviz: agent %q has an invalid point", a.ID)
			}
		}
		sort.SliceStable(a.Points, func(i, j int) bool { return a.Points[i].At.Before(a.Points[j].At) })
	}
	sort.Slice(d.Agents, func(i, j int) bool { return d.Agents[i].ID < d.Agents[j].ID })

	jobs := make(map[string]struct{}, len(d.Jobs))
	for i := range d.Jobs {
		j := &d.Jobs[i]
		j.ID = strings.TrimSpace(j.ID)
		if j.ID == "" || j.RequestedAt.IsZero() || !finite(j.Pickup.X) || !finite(j.Pickup.Y) ||
			!finite(j.Destination.X) || !finite(j.Destination.Y) {
			return errors.New("trajectoryviz: job id, requested_at, pickup, and destination are required")
		}
		if _, duplicate := jobs[j.ID]; duplicate {
			return fmt.Errorf("trajectoryviz: duplicate job %q", j.ID)
		}
		if j.FinishedAt != nil && j.FinishedAt.Before(j.RequestedAt) {
			return fmt.Errorf("trajectoryviz: job %q finishes before it was requested", j.ID)
		}
		jobs[j.ID] = struct{}{}
	}
	sort.Slice(d.Jobs, func(i, j int) bool {
		if !d.Jobs[i].RequestedAt.Equal(d.Jobs[j].RequestedAt) {
			return d.Jobs[i].RequestedAt.Before(d.Jobs[j].RequestedAt)
		}
		return d.Jobs[i].ID < d.Jobs[j].ID
	})

	for _, a := range d.Assignments {
		if _, ok := jobs[a.JobID]; !ok {
			return fmt.Errorf("trajectoryviz: assignment references unknown job %q", a.JobID)
		}
		if a.AgentID != "" {
			if _, ok := agents[a.AgentID]; !ok {
				return fmt.Errorf("trajectoryviz: assignment references unknown agent %q", a.AgentID)
			}
		}
		if a.At.IsZero() {
			return fmt.Errorf("trajectoryviz: assignment for job %q has no timestamp", a.JobID)
		}
	}
	sort.SliceStable(d.Assignments, func(i, j int) bool {
		if !d.Assignments[i].At.Equal(d.Assignments[j].At) {
			return d.Assignments[i].At.Before(d.Assignments[j].At)
		}
		return d.Assignments[i].JobID < d.Assignments[j].JobID
	})
	return nil
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }
