module signalledger

// Pinned to the patch, not just the minor. CI installs exactly what this line
// says (`setup-go` with `go-version-file`), so a bare `go 1.25` left it building
// on the .0 release and govulncheck flagged 21 standard-library advisories that
// no dependency of ours introduced. The patch level is a security floor: keep it
// current rather than rounding it back down to a minor.
go 1.26.5

require github.com/jackc/pgx/v5 v5.10.0

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/text v0.39.0 // indirect
)
