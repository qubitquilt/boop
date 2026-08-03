// Package store is the persistent record of every Boop review run and
// the LLM telemetry that goes with it. It backs the dashboard data
// layer (GET /api/runs, GET /api/stats) and is the source of truth for
// historical analysis — the K8s Job that ran a review is GC'd 1 hour
// after it finishes, so the store has to out-live the Job for the
// dashboard to show anything older than that.
//
// Storage is a single SQLite file. SQLite is the right call here: the
// write rate is one row per review (a few per minute at worst), the
// read pattern is aggregation queries for the dashboard plus point
// lookups for /api/runs, and a single file is trivial to back up
// (cron + `sqlite3 .backup`) and to ship to a developer's laptop for
// ad-hoc SQL. Postgres would buy us nothing for this volume and would
// add an operator, a connection pool, and a separate backup story.
//
// The driver is modernc.org/sqlite (pure Go, no CGO). The receiver
// ships as distroless/static (CGO_ENABLED=0), and the rest of the
// K8s deployment assumes that; a CGO driver would force a
// glibc-based image and break the distroless story.
package store
