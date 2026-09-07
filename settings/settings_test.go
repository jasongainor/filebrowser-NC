package settings

import "testing"

func TestJobsConfig_DefaultsToBucketedOn(t *testing.T) {
	var c JobsConfig // zero-valued, e.g. an existing config.json with no "jobs" key
	if !c.RootIsJobsResolved() {
		t.Errorf("zero-valued JobsConfig should resolve RootIsJobs=true")
	}
	if !c.AutoBucketResolved() {
		t.Errorf("zero-valued JobsConfig should resolve AutoBucket=true")
	}
}

func TestJobsConfig_ExplicitFalseHonored(t *testing.T) {
	f := false
	c := JobsConfig{RootIsJobs: &f}
	if c.RootIsJobsResolved() {
		t.Errorf("RootIsJobs=false should resolve false")
	}
	if c.AutoBucketResolved() {
		t.Errorf("AutoBucket should also resolve false when RootIsJobs is off, regardless of its own value")
	}
}

func TestJobsConfig_AutoBucketFalseAlone(t *testing.T) {
	trueVal, falseVal := true, false
	c := JobsConfig{RootIsJobs: &trueVal, AutoBucket: &falseVal}
	if !c.RootIsJobsResolved() {
		t.Errorf("RootIsJobs=true should resolve true")
	}
	if c.AutoBucketResolved() {
		t.Errorf("AutoBucket=false should resolve false even with RootIsJobs on")
	}
}

func TestJobsConfig_ExplicitTrueHonored(t *testing.T) {
	trueVal := true
	c := JobsConfig{RootIsJobs: &trueVal, AutoBucket: &trueVal}
	if !c.RootIsJobsResolved() || !c.AutoBucketResolved() {
		t.Errorf("explicit true/true should resolve on/on")
	}
}
