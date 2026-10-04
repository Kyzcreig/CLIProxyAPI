package resetweighted

import (
	"testing"
	"time"
)

func TestParseCodexUsage(t *testing.T) {
	body := []byte(`{"plan_type":"pro","rate_limit":{"limit_reached":false,
	 "primary_window":{"used_percent":21,"limit_window_seconds":604800,"reset_at":1791606464},
	 "secondary_window":{"used_percent":4,"limit_window_seconds":18000,"reset_at":1791100000}}}`)
	q, ok := ParseCodexUsage(body, t0)
	if !ok || q.Long.Used != 21 || q.Long.Span != spanWeek || q.Short.Used != 4 || q.Short.Span != span5h {
		t.Fatalf("codex = %+v ok=%v", q, ok)
	}
	if q.Long.ResetsAt != time.Unix(1791606464, 0).UTC() {
		t.Fatalf("resets_at = %v", q.Long.ResetsAt)
	}
	// Vendor may label the 5h window primary; sort by span.
	swapped := []byte(`{"rate_limit":{"primary_window":{"used_percent":4,"limit_window_seconds":18000},"secondary_window":{"used_percent":21,"limit_window_seconds":604800}}}`)
	q, _ = ParseCodexUsage(swapped, t0)
	if q.Long.Used != 21 || q.Short.Used != 4 {
		t.Fatalf("span sort: %+v", q)
	}
	// limit_reached at 100 -> rejected
	capped := []byte(`{"rate_limit":{"limit_reached":true,"primary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_at":1791606464}}}`)
	q, _ = ParseCodexUsage(capped, t0)
	if !q.Long.Rejected || !Exhausted(q, t0) {
		t.Fatalf("capped codex: %+v", q)
	}
	if _, ok := ParseCodexUsage([]byte(`{}`), t0); ok {
		t.Fatal("empty body must not parse")
	}
}

func TestParseXAIBilling(t *testing.T) {
	body := []byte(`{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-09-17T21:32:33.935584+00:00","end":"2026-09-24T21:32:33.935584+00:00"},"creditUsagePercent":68.0,"productUsage":[{"product":"GrokBuild","usagePercent":68.0}]}}`)
	q, ok := ParseXAIBilling(body, t0)
	if !ok || q.Long.Used != 68 || q.Short.Known {
		t.Fatalf("xai = %+v ok=%v", q, ok)
	}
	if q.Long.Span != 7*24*time.Hour || q.Long.ResetsAt.Format(time.RFC3339) != "2026-09-24T21:32:33Z" {
		t.Fatalf("span=%v resets=%v", q.Long.Span, q.Long.ResetsAt)
	}
}

func TestParseKimiUsages(t *testing.T) {
	body := []byte(`{"limits":[{"window":{"duration":300,"timeUnit":"TIME_UNIT_MINUTE"},"detail":{"limit":"100","used":"31","remaining":"69","resetTime":"2026-10-04T07:11:25.612548Z"}}],
	 "usages":{"limit_5h":{"used_ratio":0,"reset_time":"2026-10-04T07:11:24Z"},"limit_month_total":{"used_ratio":0.4547,"reset_time":"2026-10-29T00:00:00Z"}}}`)
	q, ok := ParseKimiUsages(body, t0)
	if !ok || q.Long.Span != spanMonth || q.Long.Used < 45.46 || q.Long.Used > 45.48 || q.Short.Used != 31 {
		t.Fatalf("kimi = %+v ok=%v", q, ok)
	}
	if q.Long.ResetsAt.Format(time.RFC3339) != "2026-10-29T00:00:00Z" {
		t.Fatalf("month reset = %v", q.Long.ResetsAt)
	}
}

func TestParseAntigravityQuota(t *testing.T) {
	body := []byte(`{"groups":[{"name":"Gemini Models","buckets":[
	 {"bucket_id":"gemini-weekly","window":"weekly","remaining_fraction":0.8937862,"reset_time":"2026-10-07T02:44:17Z","disabled":false},
	 {"bucket_id":"gemini-5h","window":"5h","remaining_fraction":0.9705428,"reset_time":"2026-10-04T04:30:17Z","disabled":false}]},
	 {"name":"Claude and GPT models","buckets":[{"bucket_id":"3p-weekly","remaining_fraction":1}]}]}`)
	q, ok := ParseAntigravityQuota(body, t0)
	if !ok || q.Long.Used < 10.6 || q.Long.Used > 10.7 || q.Short.Used < 2.9 || q.Short.Used > 3.0 {
		t.Fatalf("antigravity = %+v ok=%v", q, ok)
	}
}

func TestParseUsageAceClaude(t *testing.T) {
	body := []byte(`{"schema":8,"providers":[
	 {"id":"codex","accounts":[{"key":"codex:97ff","windows":[{"key":"7-day","pct":21}]}]},
	 {"id":"claude","accounts":[
	   {"key":"sub-vps-6","observed_at":1791085825.094,"windows":[
	     {"key":"five_hour","pct":14.0,"resets_at":"2026-10-04T06:59:59.724163+00:00","status":"allowed","observed_at":"2026-10-04T03:35:22.573662+00:00"},
	     {"key":"seven_day","pct":55.0,"resets_at":"2026-10-09T09:59:59.724183+00:00","status":"allowed","observed_at":"2026-10-04T03:35:22.573662+00:00"},
	     {"key":"seven_day_overage_included","pct":75.0,"resets_at":"2026-10-09T09:59:59.724339+00:00","status":"allowed","stale":false,"scoped":true}]},
	   {"key":"local","windows":[
	     {"key":"five_hour","pct":0,"resets_at":"2026-10-04T08:00:00.000Z","status":"allowed"},
	     {"key":"seven_day","pct":100,"resets_at":"2026-10-05T06:00:00.000Z","status":"rejected"},
	     {"key":"seven_day_overage_included","pct":94,"resets_at":"2026-10-05T06:00:00.000Z","status":"allowed_warning","stale":true}]},
	   {"key":"nowindows","windows":[]}]}]}`)
	rows := ParseUsageAceClaude(body, t0)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (nowindows skipped)", len(rows))
	}
	byKey := map[string]*Quota{}
	for _, r := range rows {
		byKey[r.Key] = r.Quota
	}
	sub6 := byKey["sub-vps-6"]
	if sub6.Long.Used != 55 || sub6.Short.Used != 14 || !sub6.Fable.Known || sub6.Fable.Used != 75 {
		t.Fatalf("sub-vps-6 = %+v", sub6)
	}
	if sub6.ObservedAt.Format(time.RFC3339) != "2026-10-04T03:35:22Z" {
		t.Fatalf("observed_at = %v (oldest window observation wins)", sub6.ObservedAt)
	}
	local := byKey["local"]
	if !local.Long.Rejected || !Exhausted(local, t0) || local.Fable.Known {
		t.Fatalf("local = %+v, want rejected weekly and stale fable dropped", local)
	}
}
