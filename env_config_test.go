package main

import (
	"testing"
	"time"
)

func TestParseBackends(t *testing.T) {
	got, err := ParseBackends("A-1:8081:1, A-2:8082:1,A-3:8083:2")
	if err != nil || len(got) != 3 || got[2].Weight != 2 || got[1].Name != "A-2" || got[1].Port != "8082" {
		t.Fatalf("%+v %v", got, err)
	}
	for _, bad := range []string{"", "A:1", "A:8081:0", "A:8081:x", "A:0:1", "A:99999:1", ":8081:1", "A:8081:1,A:8082:1",
		"A:8081:1,B:8081:1", "A B:8081:1", "A:8081:1001"} {
		if _, err := ParseBackends(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestBackendsFromEnvDefaultAndOverride(t *testing.T) {
	d, err := backendsFromEnv()
	if err != nil || len(d) != 3 || d[0].Weight != 3 {
		t.Fatalf("default pool wrong: %+v", d)
	}
	t.Setenv("LB_BACKENDS", "X:9001:1,Y:9002:1,Z:9003:1")
	o, _ := backendsFromEnv()
	if len(o) != 3 || o[0].Name != "X" || o[2].Weight != 1 {
		t.Fatalf("%+v", o)
	}
	t.Setenv("LB_BACKENDS", "bad")
	if _, err := backendsFromEnv(); err == nil {
		t.Fatal("invalid LB_BACKENDS must be an error")
	}
}

func TestRuleControllerEnv(t *testing.T) {
	cfg, iv := ruleControllerFromEnv()
	if iv != 10*time.Second || cfg.EMAAlpha != 0.3 || cfg.Deadband != 0.02 {
		t.Fatalf("defaults changed: %+v %v", cfg, iv)
	}
	t.Setenv("LB_CONTROLLER_INTERVAL_S", "3")
	t.Setenv("LB_CONTROLLER_EMA_ALPHA", "1")
	t.Setenv("LB_CONTROLLER_DEADBAND", "0")
	cfg, iv = ruleControllerFromEnv()
	if iv != 3*time.Second || cfg.EMAAlpha != 1 || cfg.Deadband != 0 {
		t.Fatalf("ablation settings not applied: %+v %v", cfg, iv)
	}
	t.Setenv("LB_CONTROLLER_EMA_ALPHA", "7") // out of range -> ignored
	if cfg, _ = ruleControllerFromEnv(); cfg.EMAAlpha != 0.3 {
		t.Fatal("invalid alpha must be ignored")
	}
}
