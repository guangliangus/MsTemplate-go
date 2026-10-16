package taskqueue

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

// MisfirePolicy decides what a cron firing nobody was there for does.
type MisfirePolicy string

const (
	// MisfireFireOnceNow runs the late firing once, then resumes from the next occurrence after now.
	MisfireFireOnceNow MisfirePolicy = "fire_once_now"
	// MisfireSkip does not run the late firing and resumes from the next occurrence after now.
	MisfireSkip MisfirePolicy = "skip"
	// MisfireCatchUp runs the late firing and schedules the occurrence after IT, so every missed
	// firing is replayed in turn.
	MisfireCatchUp MisfirePolicy = "catch_up"
)

var cronParser = cron.NewParser(
	cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// CronSchedule is a parsed wall-clock schedule.
type CronSchedule struct {
	Expression string
	Location   *time.Location
	Misfire    MisfirePolicy
	schedule   cron.Schedule
}

// ParseCron parses a five- or six-field expression (or a descriptor such as @hourly) in the IANA
// time zone (empty = UTC) with the misfire policy (empty = fire_once_now).
func ParseCron(expression, timeZone string, misfire MisfirePolicy) (*CronSchedule, error) {
	schedule, err := cronParser.Parse(expression)
	if err != nil {
		return nil, fmt.Errorf("cron expression %q: %w", expression, err)
	}
	loc := time.UTC
	if timeZone != "" {
		loc, err = time.LoadLocation(timeZone)
		if err != nil {
			return nil, fmt.Errorf("time zone %q: %w", timeZone, err)
		}
	}
	switch misfire {
	case "":
		misfire = MisfireFireOnceNow
	case MisfireFireOnceNow, MisfireSkip, MisfireCatchUp:
	default:
		return nil, fmt.Errorf("misfire policy %q is not fire_once_now, skip or catch_up", misfire)
	}
	return &CronSchedule{Expression: expression, Location: loc, Misfire: misfire, schedule: schedule}, nil
}

// NextAfter is the first occurrence strictly after t.
func (c *CronSchedule) NextAfter(t time.Time) time.Time {
	return c.schedule.Next(t.In(c.Location)).UTC()
}

// NextFireTime is the occurrence to re-arm to after a firing scheduled for scheduledFor ran at
// now: the next after now, or, under catch_up, the next after the firing itself (which may
// already be in the past, making the row due again at once).
func (c *CronSchedule) NextFireTime(scheduledFor, now time.Time) time.Time {
	if c.Misfire == MisfireCatchUp {
		return c.NextAfter(scheduledFor)
	}
	return c.NextAfter(now)
}
