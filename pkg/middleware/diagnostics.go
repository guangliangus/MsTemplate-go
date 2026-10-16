package middleware

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"example.com/msvcservice/pkg/telemetry"
)

// unmatchedRoute labels problems written before routing matched anything (a 404, a 405).
const unmatchedRoute = "unmatched"

// problems is exported as msvc_http_problems_total{http_route,http_response_status_code,error_code}:
// the status-keyed http_server_request_duration_seconds says how many requests failed, this one says
// with which code, so an alert can watch one code without a per-route status rule. error_code is a
// bounded label (telemetry.BoundedLabel); an unexpected shape lands in OTHER rather than in a new
// series.
var problems = sync.OnceValue(func() metric.Int64Counter {
	c, _ := otel.Meter("msvc/http").Int64Counter("msvc.http.problems",
		metric.WithDescription("Problem responses written, by route, status and errorCode."))
	return c
})

func recordProblem(ctx context.Context, route string, status int, code string) {
	if route == "" {
		route = unmatchedRoute
	}
	problems().Add(ctx, 1, metric.WithAttributes(
		attribute.String("http.route", route),
		attribute.Int("http.response.status_code", status),
		attribute.String("error_code", telemetry.BoundedLabel(code, "OTHER"))))
}
