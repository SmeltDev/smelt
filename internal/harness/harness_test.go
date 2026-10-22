package harness

import (
	"context"
	"errors"
	"testing"
	"time"

	"gotest.tools/v3/assert"
)

func TestTimeoutError(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
		want    string
	}{
		{"with a run timeout", 30 * time.Second, "timed out after 30s"},
		{"without one", 0, "timed out: context deadline exceeded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := TimeoutError(tt.timeout, context.DeadlineExceeded)
			assert.Equal(t, err.Error(), tt.want)
			if tt.timeout == 0 {
				assert.Assert(t, errors.Is(err, context.DeadlineExceeded))
			}
		})
	}
}

func TestAtCapacityIsOnlyTheCapacityKindOfUnavailable(t *testing.T) {
	err := AtCapacity(errors.New("Selected model is at capacity"))
	var f *Failure
	assert.Assert(t, errors.As(err, &f))
	assert.Equal(t, f.Kind, Unavailable)
	assert.Assert(t, IsAtCapacity(err))
	assert.Equal(t, err.Error(), "Selected model is at capacity")
	assert.Assert(t, !IsAtCapacity(Classified(Unavailable, errors.New("missing"), nil)))
}
