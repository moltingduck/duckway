package handlers

import "testing"

func TestRestoreCCMaskedConfigUnparseableStoredDropsMask(t *testing.T) {
	got := restoreCCMaskedConfig(`{"bot_token":"********","name":"x"}`, `not json`)
	if got != `{"name":"x"}` {
		t.Fatalf("got %s, want mask dropped and name kept", got)
	}
}

func TestRestoreCCMaskedConfigArrayIsIndexBased(t *testing.T) {
	got := restoreCCMaskedConfig(`{"hooks":[{"secret":"********"}]}`, `{"hooks":[{"secret":"s0"}]}`)
	if got != `{"hooks":[{"secret":"s0"}]}` {
		t.Fatalf("got %s", got)
	}
}
