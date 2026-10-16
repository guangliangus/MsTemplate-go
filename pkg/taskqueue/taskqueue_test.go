package taskqueue

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestParseCronFiveAndSixFields(t *testing.T) {
	five, err := ParseCron("*/5 * * * *", "", "")
	if err != nil {
		t.Fatal(err)
	}
	six, err := ParseCron("0 */5 * * * *", "UTC", MisfireSkip)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 1, 10, 2, 30, 0, time.UTC)
	want := time.Date(2026, 1, 1, 10, 5, 0, 0, time.UTC)
	if five.NextAfter(at) != want || six.NextAfter(at) != want {
		t.Fatalf("next: %s / %s, want %s", five.NextAfter(at), six.NextAfter(at), want)
	}
	if five.Misfire != MisfireFireOnceNow || six.Misfire != MisfireSkip {
		t.Fatal("misfire defaults")
	}
}

func TestParseCronTimeZone(t *testing.T) {
	s, err := ParseCron("0 2 * * *", "Asia/Taipei", "")
	if err != nil {
		t.Fatal(err)
	}
	// 02:00 Taipei (UTC+8) is 18:00 UTC the previous day.
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	want := time.Date(2026, 3, 1, 18, 0, 0, 0, time.UTC)
	if got := s.NextAfter(at); !got.Equal(want) {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestParseCronRefusesBadInput(t *testing.T) {
	for _, tc := range []struct {
		expr, tz string
		misfire  MisfirePolicy
	}{
		{"not a cron", "", ""},
		{"* * * * *", "Mars/Olympus", ""},
		{"* * * * *", "", "explode"},
	} {
		if _, err := ParseCron(tc.expr, tc.tz, tc.misfire); err == nil {
			t.Errorf("accepted %+v", tc)
		}
	}
}

func TestNextFireTimeByPolicy(t *testing.T) {
	scheduled := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	now := time.Date(2026, 1, 1, 10, 47, 0, 0, time.UTC) // claimed 47 minutes late
	hourly := func(p MisfirePolicy) *CronSchedule {
		s, _ := ParseCron("0 * * * *", "", p)
		return s
	}
	if got := hourly(MisfireFireOnceNow).NextFireTime(scheduled, now); got.Hour() != 11 {
		t.Fatalf("fire_once_now resumes after now: %s", got)
	}
	if got := hourly(MisfireCatchUp).NextFireTime(scheduled, now); got.Hour() != 11 || got.Minute() != 0 {
		t.Fatalf("catch_up schedules the occurrence after the firing itself: %s", got)
	}
	// Two missed firings under catch_up: the next after the FIRING (11:00) is already past "now".
	late := time.Date(2026, 1, 1, 12, 30, 0, 0, time.UTC)
	if got := hourly(MisfireCatchUp).NextFireTime(scheduled, late); !got.Before(late) {
		t.Fatalf("catch_up must make the row due again at once: %s", got)
	}
}

func TestConfigValidateEnforcesStaleAfter(t *testing.T) {
	cfg := Config{TaskTimeout: 5 * time.Minute, StaleAfter: 5 * time.Minute, RetryBaseDelay: time.Second,
		RetryMaxDelay: time.Minute}
	if err := cfg.Validate(); err == nil {
		t.Fatal("equal timeouts must be refused")
	}
	cfg.StaleAfter = 10 * time.Minute
	cfg.Cron = map[string]CronConfig{"x": {Expression: "bad"}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("a bad cron override must be refused")
	}
	cfg.Cron["x"] = CronConfig{Expression: "@hourly", Misfire: "skip"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestQueueSettingsOverrides(t *testing.T) {
	two := 2
	cfg := Config{MaxWorkers: 4, MaxAttempts: 5, ShortPollInterval: time.Second, LongPollInterval: 5 * time.Second,
		Queues: map[string]QueueOverride{"slow": {MaxWorkers: &two}}}
	if s := cfg.settingsFor("slow"); s.maxWorkers != 2 || s.maxAttempts != 5 {
		t.Fatalf("%+v", s)
	}
	if s := cfg.settingsFor("other"); s.maxWorkers != 4 {
		t.Fatalf("%+v", s)
	}
}

func TestResultConstructors(t *testing.T) {
	if Done().Action != ActionDone || Settled().Action != ActionSettled {
		t.Fatal("done/settled")
	}
	r := Retry(time.Minute, "X", "y")
	if r.Action != ActionRetry || r.RetryAfter != time.Minute || r.ErrorCode != "X" {
		t.Fatalf("%+v", r)
	}
	if Discard("X", "y").Action.String() != "discard" {
		t.Fatal("discard")
	}
}

func TestAPanickingTaskBecomesAFailureNotACrash(t *testing.T) {
	r := NewRunner(nil, Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	reg := registration{queue: "q", handler: HandlerFunc(func(context.Context, Task) (Result, error) {
		panic("nil map")
	})}
	if _, err := r.invoke(context.Background(), reg, Task{QueueName: "q"}, r.log); err == nil {
		t.Fatal("a panic is a failed run")
	}
}

func TestFailuresSpendTheBudgetOfOneOffTasksOnly(t *testing.T) {
	settings := queueSettings{maxAttempts: 3}
	oneOff := registration{queue: "q"}
	recurring := registration{queue: "r", interval: time.Minute}
	if budgetExhausted(oneOff, settings, Task{Attempt: 2}) || !budgetExhausted(oneOff, settings, Task{Attempt: 3}) {
		t.Fatal("one-off tasks die on their last attempt")
	}
	if budgetExhausted(recurring, settings, Task{Attempt: 99}) {
		t.Fatal("recurring tasks are exempt")
	}
	cfg := Config{TaskTimeout: time.Minute, StaleAfter: 2 * time.Minute, RetryBaseDelay: time.Hour, RetryMaxDelay: time.Minute}
	if err := cfg.Validate(); err == nil {
		t.Fatal("a base delay above the cap is refused")
	}
}
