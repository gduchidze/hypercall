package eval

import "encoding/json"

// boundDetails budgets escaped JSON bytes, so control characters cannot expand
// an admitted report beyond its public transport envelope. Provenance digests
// retain the identity of omitted scorer cost-source strings.
func boundDetails(a *AssertionResult, remaining *int) {
	encoded, _ := json.Marshal(a.Reason)
	if len(encoded) > *remaining {
		a.Reason = ""
		a.ReasonTruncated = true
	} else {
		*remaining -= len(encoded)
	}
	if a.Cost != nil {
		encoded, _ = json.Marshal(a.Cost.Source)
		if len(encoded) > *remaining {
			a.Cost.SourceDigest = digest(a.Cost.Source)
			a.Cost.Source = ""
			a.Cost.SourceTruncated = true
		} else {
			*remaining -= len(encoded)
		}
	}
}
