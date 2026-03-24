package pipeline_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ruffel/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// -----------------------------------------------------------------------------
// Location
// -----------------------------------------------------------------------------

func TestLocation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		loc      pipeline.Location
		wantJSON string
	}{
		{
			name:     "pipeline only",
			loc:      pipeline.Location{Pipeline: "deploy"},
			wantJSON: `{"pipeline":"deploy"}`,
		},
		{
			name:     "pipeline and stage",
			loc:      pipeline.Location{Pipeline: "deploy", Stage: "validate"},
			wantJSON: `{"pipeline":"deploy","stage":"validate"}`,
		},
		{
			name:     "all fields",
			loc:      pipeline.Location{Pipeline: "deploy", Stage: "validate", Step: "check-certs"},
			wantJSON: `{"pipeline":"deploy","stage":"validate","step":"check-certs"}`,
		},
		{
			name:     "empty omits fields",
			loc:      pipeline.Location{},
			wantJSON: `{}`,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b, err := json.Marshal(tt.loc)
			require.NoError(t, err)
			assert.JSONEq(t, tt.wantJSON, string(b))
		})
	}
}

// -----------------------------------------------------------------------------
// In-flight event constructors
// -----------------------------------------------------------------------------

func TestNewProgressEvent(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	loc := pipeline.Location{Pipeline: "p", Stage: "s", Step: "step"}
	e := pipeline.NewProgressEvent(loc, "upload", "uploading files", 3, 10, now)

	assert.Equal(t, loc, e.Location)
	assert.Equal(t, now, e.Timestamp)
	assert.Equal(t, "upload", e.Key)
	assert.Equal(t, "uploading files", e.Message)
	assert.Equal(t, 3, e.Current)
	assert.Equal(t, 10, e.Total)
}

func TestNewOutputEvent(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	loc := pipeline.Location{Pipeline: "p", Stage: "s", Step: "step"}
	e := pipeline.NewOutputEvent(loc, pipeline.Stderr, "error: not found", now)

	assert.Equal(t, loc, e.Location)
	assert.Equal(t, pipeline.Stderr, e.Stream)
	assert.Equal(t, "error: not found", e.Line)
}

func TestNewMessageEvent(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	loc := pipeline.Location{Pipeline: "p"}
	attrs := map[string]string{"host": "prod-1"}
	e := pipeline.NewMessageEvent(loc, pipeline.LevelWarn, "connection slow", attrs, now)

	assert.Equal(t, pipeline.LevelWarn, e.Level)
	assert.Equal(t, "connection slow", e.Message)
	assert.Equal(t, attrs, e.Attrs)
}

func TestNewCustomEvent(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	loc := pipeline.Location{Pipeline: "p"}
	data := map[string]string{"url": "https://example.com"}
	e := pipeline.NewCustomEvent(loc, "deploy.url-ready", data, now)

	assert.Equal(t, "deploy.url-ready", e.Type)
	assert.Equal(t, data, e.Data)
}
