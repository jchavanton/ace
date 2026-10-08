package controller

import (
	"testing"

	"github.com/jchavanton/ace/models"
)

func callWithRxFrames(frames ...int) models.CallResult {
	c := models.CallResult{Action: "call", Result: "PASS"}
	for _, f := range frames {
		var s models.RTPStats
		s.Rx.VoiceFrames = f
		c.RTPStats = append(c.RTPStats, s)
	}
	return c
}

// Each rtp_stats block carries the call's running totals, so a call counts
// once, by its last block, however many blocks it has.
func TestAggregateVoiceAveragesCallsNotBlocks(t *testing.T) {
	agg := aggregate([]models.CallResult{callWithRxFrames(20, 80), callWithRxFrames(60)})
	if want := (80 + 60) / 2 * models.SamplerPeriodMs; agg.VoiceAvgRxMs != want {
		t.Fatalf("VoiceAvgRxMs = %d, want %d (the two calls' 8 s and 6 s averaged)", agg.VoiceAvgRxMs, want)
	}
}
