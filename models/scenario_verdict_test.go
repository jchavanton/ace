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
	if ok, why := (ScenarioVerdict{MinRxVoiceMs: 5000}).Apply(rebuiltCall(20, 80), false); !ok {
		t.Fatalf("a call with 8 s of received voice failed min_rx_voice_ms=5000: %s", why)
	}
}

func TestVerdictStillFailsAQuietCall(t *testing.T) {
	if ok, _ := (ScenarioVerdict{MinRxVoiceMs: 5000}).Apply(rebuiltCall(20), false); ok {
		t.Fatal("a call with 2 s of received voice passed min_rx_voice_ms=5000")
	}
}

func TestVerdictCeilingSeesTheCallsHighestPeak(t *testing.T) {
	if ok, _ := (ScenarioVerdict{MaxRxLevelPeak: 50}).Apply(rebuiltCall(20, 80), false); ok {
		t.Fatal("a call that peaked at 80 passed max_rx_level_peak=50")
	}
}

func TestVerdictPassesWhenToneFiredInTime(t *testing.T) {
	c := &CallResult{Action: "call", Result: "PASS", ToneDetected: 1, ToneDetectedMs: 3000}
	if ok, why := (ScenarioVerdict{MaxToneDetectedMs: 5000}).Apply(c, true); !ok {
		t.Fatalf("tone at 3s with max=5s failed: %s", why)
	}
}

func TestVerdictFailsWhenToneNeverFiredButScenarioRequiresIt(t *testing.T) {
	c := &CallResult{Action: "call", Result: "PASS"}
	if ok, _ := (ScenarioVerdict{}).Apply(c, true); ok {
		t.Fatal("a call with tone_detected=0 passed under hasDetectTone=true")
	}
}

func TestVerdictIgnoresMissingToneWhenScenarioDoesntRequireIt(t *testing.T) {
	c := &CallResult{Action: "call", Result: "PASS"}
	if ok, _ := (ScenarioVerdict{}).Apply(c, false); !ok {
		t.Fatal("a plain call failed with no checks configured")
	}
}

func TestVerdictFailsWhenToneFiredTooLate(t *testing.T) {
	c := &CallResult{Action: "call", Result: "PASS", ToneDetected: 1, ToneDetectedMs: 8000}
	if ok, _ := (ScenarioVerdict{MaxToneDetectedMs: 5000}).Apply(c, true); ok {
		t.Fatal("a tone at 8s passed max_tone_detected_ms=5000")
	}
}

// XMLHasDetectTone is the single source of truth for "does this scenario
// enable the tone detector?" — both the verdict and the scenario-detail
// UI read it, so regressions here silently break the Require-tone check.
func TestXMLHasDetectToneMatchesValueNotLiteralTrue(t *testing.T) {
	cases := []struct {
		xml  string
		want bool
	}{
		{`<action type="call" detect_tone="200,3000"/>`, true},  // real voip_patrol syntax
		{`<action type="call" detect_tone="440"/>`, true},       // freq only
		{`<action type="call" detect_tone="true"/>`, true},      // historical, still truthy
		{`<action type="call" detect_tone=""/>`, false},         // empty = disabled
		{`<action type="call"/>`, false},                        // absent
		{`<action type="call" ddetect_tone="200,3000"/>`, false}, // disabled-by-typo convention
	}
	for _, tc := range cases {
		if got := XMLHasDetectTone(tc.xml); got != tc.want {
			t.Errorf("XMLHasDetectTone(%q) = %v, want %v", tc.xml, got, tc.want)
		}
	}
}
