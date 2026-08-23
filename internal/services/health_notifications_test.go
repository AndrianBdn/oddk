package services

import (
	"strings"
	"testing"

	"github.com/andrianbdn/oddk/internal/store/health"
)

// The regression this file exists for: fail_details is one comma-joined string
// holding BOTH host and instance messages, but the degraded body rendered it
// only when the host was unhealthy — and called it "Host Issues". So an
// instance failure was dropped entirely on a healthy host (the operator got
// "Broken Instances: foo" and no reason at all), and mislabelled as a host
// problem when the host was also sick.

func TestDegradedMessage_ShowsInstanceReasonOnHealthyHost(t *testing.T) {
	msg := buildDegradedMessage("db01", "2026-08-23 03:00:00 UTC", &health.HealthRecord{
		HealthyHost:     true,
		BrokenInstances: "billing",
		FailDetails:     "pg_ping_failed:billing",
	})
	if !strings.Contains(msg, "pg_ping_failed:billing") {
		t.Fatalf("the only part of the message that says WHY must survive a healthy host:\n%s", msg)
	}
	if strings.Contains(msg, "Host: unhealthy") {
		t.Fatalf("a healthy host must not be reported unhealthy:\n%s", msg)
	}
}

func TestDegradedMessage_DoesNotLabelInstanceFailuresAsHostIssues(t *testing.T) {
	msg := buildDegradedMessage("db01", "2026-08-23 03:00:00 UTC", &health.HealthRecord{
		HealthyHost:     false,
		BrokenInstances: "billing",
		FailDetails:     "cpu_sustained_high,pg_ping_failed:billing",
	})
	if strings.Contains(msg, "Host Issues") {
		t.Fatalf("the combined fail string is not host-only and must not claim to be:\n%s", msg)
	}
	if !strings.Contains(msg, "Host: unhealthy") {
		t.Fatalf("an unhealthy host must still be reported:\n%s", msg)
	}
	if !strings.Contains(msg, "cpu_sustained_high") || !strings.Contains(msg, "pg_ping_failed:billing") {
		t.Fatalf("both reasons must reach the operator:\n%s", msg)
	}
}

func TestDegradedMessage_ExplainsErrorStateOnlyWhenPresent(t *testing.T) {
	withError := buildDegradedMessage("db01", "t", &health.HealthRecord{
		HealthyHost:     true,
		BrokenInstances: "billing",
		FailDetails:     instanceErrorPrefix + "billing",
	})
	if !strings.Contains(withError, "snapshot apply") || !strings.Contains(withError, "oddk checklist") {
		t.Fatalf("an error-state instance needs the hint that separates a failed operation from a DR leftover:\n%s", withError)
	}

	pingOnly := buildDegradedMessage("db01", "t", &health.HealthRecord{
		HealthyHost:     true,
		BrokenInstances: "billing",
		FailDetails:     "pg_ping_failed:billing",
	})
	if strings.Contains(pingOnly, "snapshot apply") {
		t.Fatalf("a cluster that stopped answering is not an error-state instance; the hint must not fire:\n%s", pingOnly)
	}
}

func TestDegradedMessage_OmitsEmptySections(t *testing.T) {
	msg := buildDegradedMessage("db01", "t", &health.HealthRecord{HealthyHost: true})
	for _, unwanted := range []string{"Broken Instances:", "Details:", "Host: unhealthy"} {
		if strings.Contains(msg, unwanted) {
			t.Fatalf("empty section %q must be omitted:\n%s", unwanted, msg)
		}
	}
}
