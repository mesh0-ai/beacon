package main

import (
	"reflect"
	"testing"
)

func TestParseInterval(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", defaultInterval},
		{"30", 30},
		{"0", minInterval},
		{"3", minInterval},
		{"5", 5},
		{"300", 300},
		{"99999", maxInterval},
		{"garbage", defaultInterval},
	}
	for _, c := range cases {
		if got := parseInterval(c.in); got != c.want {
			t.Errorf("parseInterval(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseLabels(t *testing.T) {
	got, warns := parseLabels("env=prod, team=platform ,region=us-east-1")
	want := map[string]string{
		"env": "prod", "team": "platform", "region": "us-east-1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("labels = %v, want %v", got, want)
	}
	if len(warns) != 0 {
		t.Errorf("unexpected warns: %v", warns)
	}
}

func TestParseLabels_Empty(t *testing.T) {
	got, warns := parseLabels("")
	if len(got) != 0 || len(warns) != 0 {
		t.Errorf("expected empty, got %v / %v", got, warns)
	}
}

func TestParseLabels_Reserved(t *testing.T) {
	got, warns := parseLabels("env=prod,pod_name=hijacked,cpu_pct=1")
	if got["env"] != "prod" {
		t.Errorf("missing env: %v", got)
	}
	if _, ok := got["pod_name"]; ok {
		t.Errorf("reserved pod_name should be dropped")
	}
	if _, ok := got["cpu_pct"]; ok {
		t.Errorf("reserved cpu_pct should be dropped")
	}
	if len(warns) != 2 {
		t.Errorf("want 2 warns, got %d: %v", len(warns), warns)
	}
}

func TestParseLabels_Malformed(t *testing.T) {
	got, _ := parseLabels("=novalue,nokey,k=v,,onlykey=")
	want := map[string]string{"k": "v", "onlykey": ""}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
