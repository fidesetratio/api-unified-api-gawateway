package service

import "testing"

func TestWithReferenceFillsOnlyReturnURLs(t *testing.T) {
	in := map[string]any{
		"return_url": "http://h/api/v1.0/return?ref={reference}",
		"cancel_url": "http://h/cancel?ref={reference}",
		"note":       "{reference}",
		"n":          5,
	}
	out := withReference(in, "abc")
	if out["return_url"] != "http://h/api/v1.0/return?ref=abc" || out["cancel_url"] != "http://h/cancel?ref=abc" {
		t.Errorf("urls not filled: %v", out)
	}
	if out["note"] != "{reference}" || out["n"] != 5 {
		t.Errorf("other settings must be untouched: %v", out)
	}
	if in["return_url"] != "http://h/api/v1.0/return?ref={reference}" {
		t.Error("input map was modified")
	}
}
