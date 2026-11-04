// See LICENSE for applicable terms.
package eval

import (
	"fmt"
	"math"
	"strings"
	"time"
)

type Policy struct {
	TimeoutSeconds  int      `json:"timeout_seconds,omitempty" yaml:"timeout_seconds,omitempty"`
	MaxScoreDrop    *float64 `json:"max_score_drop,omitempty" yaml:"max_score_drop,omitempty"`
	MinimumSamples  int      `json:"minimum_samples,omitempty" yaml:"minimum_samples,omitempty"`
	MaxCostMicroUSD *int64   `json:"max_cost_micro_usd,omitempty" yaml:"max_cost_micro_usd,omitempty"`
	MaxLatencyMS    *int64   `json:"max_latency_ms,omitempty" yaml:"max_latency_ms,omitempty"`
	MinimumPassRate *float64 `json:"minimum_pass_rate,omitempty" yaml:"minimum_pass_rate,omitempty"`
}
type Cost struct {
	MicroUSD    int64  `json:"micro_usd" yaml:"micro_usd"`
	Kind        string `json:"kind" yaml:"kind"`
	PriceDigest string `json:"price_digest" yaml:"price_digest"`
	RecordedAt  string `json:"recorded_at" yaml:"recorded_at"`
}
type SharedPrefix struct {
	ID   string `json:"id" yaml:"id"`
	Cost *Cost  `json:"cost" yaml:"cost"`
}
type Measurements struct {
	LatencyMS *int64 `json:"latency_ms,omitempty"`
	Cost      *Cost  `json:"cost,omitempty"`
}
type CandidateSummary struct {
	ID            string   `json:"id"`
	Expected      int      `json:"expected"`
	Completed     int      `json:"completed"`
	Passed        int      `json:"passed"`
	PassRate      float64  `json:"pass_rate"`
	MeanScore     float64  `json:"mean_score"`
	WilsonLower   float64  `json:"wilson_95_lower"`
	WilsonUpper   float64  `json:"wilson_95_upper"`
	CostMicroUSD  *int64   `json:"cost_micro_usd,omitempty"`
	MeanLatencyMS *float64 `json:"mean_latency_ms,omitempty"`
	Qualified     bool     `json:"qualified"`
}

func validCost(c *Cost) error {
	if c == nil {
		return nil
	}
	if c.MicroUSD < 0 || !pin.MatchString(c.PriceDigest) {
		return fmt.Errorf("nonnegative microUSD and immutable price_digest required")
	}
	if c.Kind != "synthetic" && c.Kind != "measured" && c.Kind != "estimated" {
		return fmt.Errorf("cost kind must be synthetic, measured or estimated")
	}
	if _, err := time.Parse(time.RFC3339, c.RecordedAt); err != nil {
		return fmt.Errorf("timestamped cost required")
	}
	return nil
}
func addReason(r *Report, reason string) {
	for _, x := range r.CI.Reasons {
		if x == reason {
			return
		}
	}
	r.CI.Reasons = append(r.CI.Reasons, reason)
}
func finishReport(p *Plan, r *Report) {
	r.CompatibilityDigest = PlanCompatibilityDigest(p)
	r.OutputSelector = p.Config.OutputSelector
	r.BaselineReportID = p.Config.BaselineReportID
	r.SharedPrefix = p.Config.SharedPrefix
	r.WorkloadDigest = p.Config.WorkloadDigest
	r.Synthetic = true
	r.Pareto = []string{}
	if r.Expected > 0 {
		r.PassRate = float64(r.Passed) / float64(r.Expected)
	}
	var total int64
	known := p.Config.Scope != "continuation" || p.Config.SharedPrefix != nil
	if prefix := p.Config.SharedPrefix; prefix != nil {
		if prefix.Cost == nil {
			known = false
		} else {
			total = prefix.Cost.MicroUSD
		}
	}
	minimum := p.Config.Policy.MinimumSamples
	if minimum == 0 {
		minimum = 1
	}
	for _, candidate := range p.Config.Candidates {
		s := CandidateSummary{ID: candidate.ID}
		costKnown, latencyKnown := true, true
		var cost, latency int64
		for _, cell := range r.Cells {
			if cell.CandidateID != candidate.ID {
				continue
			}
			s.Expected++
			if cell.Source != "" && cell.Source != "synthetic" && !cell.Fixture {
				r.Synthetic = false
			}
			if cell.State == "complete" {
				s.Completed++
				r.ScoreSamples++
				s.MeanScore += cell.Score
			}
			if cell.Pass {
				s.Passed++
			}
			if cell.Measurements == nil || cell.Measurements.Cost == nil {
				costKnown = false
				known = false
			} else {
				amount := cell.Measurements.Cost.MicroUSD
				if amount > math.MaxInt64-total || amount > math.MaxInt64-cost {
					known = false
					costKnown = false
					addReason(r, "measurement_error")
				} else {
					total += amount
					cost += amount
				}
			}
			if cell.Measurements == nil || cell.Measurements.LatencyMS == nil {
				latencyKnown = false
			} else {
				v := *cell.Measurements.LatencyMS
				if v > math.MaxInt64-latency {
					latencyKnown = false
				} else {
					latency += v
				}
				if p.Config.Policy.MaxLatencyMS != nil && v > *p.Config.Policy.MaxLatencyMS {
					addReason(r, "budget_exceeded")
				}
			}
		}
		if s.Completed > 0 {
			s.MeanScore /= float64(s.Completed)
		}
		if s.Expected > 0 {
			s.PassRate = float64(s.Passed) / float64(s.Expected)
			n := float64(s.Expected)
			z := 1.959963984540054
			den := 1 + z*z/n
			center := (s.PassRate + z*z/(2*n)) / den
			margin := z * math.Sqrt(s.PassRate*(1-s.PassRate)/n+z*z/(4*n*n)) / den
			s.WilsonLower = math.Max(0, center-margin)
			s.WilsonUpper = math.Min(1, center+margin)
		}
		if costKnown {
			s.CostMicroUSD = &cost
		}
		if latencyKnown && s.Expected > 0 {
			v := float64(latency) / float64(s.Expected)
			s.MeanLatencyMS = &v
		}
		quality := 1.0
		if p.Config.Policy.MinimumPassRate != nil {
			quality = *p.Config.Policy.MinimumPassRate
		}
		s.Qualified = s.Completed == s.Expected && s.Expected >= minimum && s.PassRate >= quality
		if s.Expected < minimum {
			addReason(r, "insufficient_samples")
		}
		if s.PassRate < quality {
			addReason(r, "quality_regression")
		}
		if p.Baseline != nil {
			found := false
			for _, baseline := range p.Baseline.Candidates {
				if baseline.ID == s.ID {
					found = true
					drop := 0.0
					if p.Config.Policy.MaxScoreDrop != nil {
						drop = *p.Config.Policy.MaxScoreDrop
					}
					if s.MeanScore+drop < baseline.MeanScore {
						addReason(r, "quality_regression")
					}
				}
			}
			if !found {
				addReason(r, "evaluation_error")
			}
		}
		r.Candidates = append(r.Candidates, s)
	}
	if known {
		v := total
		r.ExecutionCostMicroUSD = &v
	}
	var grading int64
	gradingKnown := true
	gradingByCandidate := map[string]int64{}
	gradingUnknown := map[string]bool{}
	for i, cell := range r.Cells {
		for j, a := range p.Cells[i].Assertions {
			if a.Type != "python" && a.Type != "llm-judge" {
				continue
			}
			if j >= len(cell.Assertions) || cell.Assertions[j].Cost == nil {
				gradingKnown = false
				gradingUnknown[cell.CandidateID] = true
				continue
			}
			c := cell.Assertions[j].Cost
			if !finite(c.Amount) || c.Amount < 0 || c.Currency != "USD" || (c.Kind != "synthetic" && c.Kind != "estimated" && c.Kind != "measured") || (c.Source == "" && !c.SourceTruncated) || c.Amount*1e6 >= float64(math.MaxInt64) {
				gradingKnown = false
				gradingUnknown[cell.CandidateID] = true
				addReason(r, "measurement_error")
				continue
			}
			amount := int64(math.Round(c.Amount * 1e6))
			if amount > math.MaxInt64-grading {
				gradingKnown = false
				gradingUnknown[cell.CandidateID] = true
				addReason(r, "measurement_error")
				continue
			}
			grading += amount
			gradingByCandidate[cell.CandidateID] += amount
		}
	}
	for i := range r.Candidates {
		s := &r.Candidates[i]
		if gradingUnknown[s.ID] {
			s.CostMicroUSD = nil
			continue
		}
		if s.CostMicroUSD != nil {
			added := gradingByCandidate[s.ID]
			if added > math.MaxInt64-*s.CostMicroUSD {
				s.CostMicroUSD = nil
				addReason(r, "measurement_error")
			} else {
				v := *s.CostMicroUSD + added
				s.CostMicroUSD = &v
			}
		}
	}
	if gradingKnown {
		r.ScoringCostMicroUSD = &grading
	}
	if known && gradingKnown && grading <= math.MaxInt64-total {
		total += grading
		r.TotalCostMicroUSD = &total
	} else {
		known = false
	}
	if p.Config.Policy.MaxCostMicroUSD != nil {
		if !known {
			addReason(r, "measurement_missing")
		} else if total > *p.Config.Policy.MaxCostMicroUSD {
			addReason(r, "budget_exceeded")
		}
	}
	if p.Config.Policy.MaxLatencyMS != nil {
		for _, s := range r.Candidates {
			if s.MeanLatencyMS == nil {
				addReason(r, "measurement_missing")
			}
		}
	}
	for _, a := range r.Candidates {
		if !a.Qualified || a.CostMicroUSD == nil || a.MeanLatencyMS == nil {
			continue
		}
		dominated := false
		for _, b := range r.Candidates {
			if a.ID == b.ID || !b.Qualified || b.CostMicroUSD == nil || b.MeanLatencyMS == nil {
				continue
			}
			if b.MeanScore >= a.MeanScore && *b.CostMicroUSD <= *a.CostMicroUSD && *b.MeanLatencyMS <= *a.MeanLatencyMS && (b.MeanScore > a.MeanScore || *b.CostMicroUSD < *a.CostMicroUSD || *b.MeanLatencyMS < *a.MeanLatencyMS) {
				dominated = true
			}
		}
		if !dominated {
			r.Pareto = append(r.Pareto, a.ID)
		}
	}
	r.CI.Pass = len(r.CI.Reasons) == 0
}

func CompatibilityDigest(c Config) string {
	prefix := ""
	if c.SharedPrefix != nil {
		prefix = c.SharedPrefix.ID
	}
	return digest(struct {
		Scope, Workload, Cache, Prefix string
		Cases                          []Case
		Repetitions                    int
	}{c.Scope, c.WorkloadDigest, c.CachePolicy, prefix, c.Cases, c.Repetitions})
}
func PlanCompatibilityDigest(p *Plan) string {
	return digest([]string{CompatibilityDigest(p.Config), p.EvidenceDigest})
}

func Markdown(r *Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Evaluation report\n\nScope: %s. Synthetic fixture measurements: %t.\n\n%d/%d passed; score %.4f; CI pass: %t.\n\n", r.Scope, r.Synthetic, r.Passed, r.Expected, r.MeanScore, r.CI.Pass)
	if r.TotalCostMicroUSD == nil {
		b.WriteString("Total cost: unknown.\n\n")
	} else {
		fmt.Fprintf(&b, "Total cost: %d microUSD (see per-cell provenance).\n\n", *r.TotalCostMicroUSD)
	}
	b.WriteString("| Candidate | Passed / expected | Mean score | 95% Wilson interval | Cost microUSD |\n| --- | ---: | ---: | --- | ---: |\n")
	for _, s := range r.Candidates {
		cost := "unknown"
		if s.CostMicroUSD != nil {
			cost = fmt.Sprint(*s.CostMicroUSD)
		}
		fmt.Fprintf(&b, "| %s | %d / %d | %.4f | %.4f–%.4f | %s |\n", s.ID, s.Passed, s.Expected, s.MeanScore, s.WilsonLower, s.WilsonUpper, cost)
	}
	fmt.Fprintf(&b, "\nCI reasons: %s\n\nPareto candidates: %s\n", strings.Join(r.CI.Reasons, ", "), strings.Join(r.Pareto, ", "))
	for _, cell := range r.Cells {
		if cell.BranchID != "" {
			fmt.Fprintf(&b, "\nTrace: branch `%s`, call `%s`, cell `%s`.\n", cell.BranchID, cell.CallID, cell.CellID)
		}
	}
	return b.String()
}
