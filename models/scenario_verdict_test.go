package models

import "testing"

// voip_patrol writes its per-call running energy totals into every
// rtp_stats block (one per media stream torn down), so a call whose stream
// was rebuilt mid-call has a partial first block and a complete last one.
func rebuiltCall(rxFrames ...int) *CallResult {
	c := &CallResult{Action: "call", Result: "PASS"}
	for _, f := range rxFrames {
		var s RTPStats
		s.Rx.VoiceFrames = f
		s.Rx.LevelPeak = f
		c.RTPStats = append(c.RTPStats, s)
	}
	return c
}

func TestVerdictJudgesACallByItsLastRTPStatsBlock(t *testing.T) {
	if ok, why := (ScenarioVerdict{MinRxVoiceMs: 5000}).Apply(rebuiltCall(20, 80)); !ok {
		t.Fatalf("a call with 8 s of received voice failed min_rx_voice_ms=5000: %s", why)
	}
}

func TestVerdictStillFailsAQuietCall(t *testing.T) {
	if ok, _ := (ScenarioVerdict{MinRxVoiceMs: 5000}).Apply(rebuiltCall(20)); ok {
		t.Fatal("a call with 2 s of received voice passed min_rx_voice_ms=5000")
	}
}

func TestVerdictCeilingSeesTheCallsHighestPeak(t *testing.T) {
	if ok, _ := (ScenarioVerdict{MaxRxLevelPeak: 50}).Apply(rebuiltCall(20, 80)); ok {
		t.Fatal("a call that peaked at 80 passed max_rx_level_peak=50")
	}
}

func TestVerdictPassesWhenToneFiredInTime(t *testing.T) {
	c := &CallResult{Action: "call", Result: "PASS", ToneDetected: 1, ToneDetectedMs: 3000}
	v := ScenarioVerdict{ToneDetectedRequired: true, MaxToneDetectedMs: 5000}
	if ok, why := v.Apply(c); !ok {
		t.Fatalf("tone at 3s with max=5s failed: %s", why)
	}
}

func TestVerdictFailsWhenToneNeverFired(t *testing.T) {
	c := &CallResult{Action: "call", Result: "PASS"}
	if ok, _ := (ScenarioVerdict{ToneDetectedRequired: true}).Apply(c); ok {
		t.Fatal("a call with tone_detected=0 passed tone_detected_required")
	}
}

func TestVerdictFailsWhenToneFiredTooLate(t *testing.T) {
	c := &CallResult{Action: "call", Result: "PASS", ToneDetected: 1, ToneDetectedMs: 8000}
	if ok, _ := (ScenarioVerdict{MaxToneDetectedMs: 5000}).Apply(c); ok {
		t.Fatal("a tone at 8s passed max_tone_detected_ms=5000")
	}
}
