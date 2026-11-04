package scorer_test

import (
	"context"
	"testing"

	"github.com/gduchidze/hypercall/internal/scorer"
)

func TestUnpinnedScorerCannotExecute(t *testing.T) {
	r := scorer.NewRunner(scorer.Config{})
	result, err := r.Evaluate(context.Background(), scorer.Request{Output: "hello"})
	if err == nil || result.Pass || result.ErrorCode != "invalid_configuration" {
		t.Fatalf("unpinned scorer result = %+v, %v", result, err)
	}
}
