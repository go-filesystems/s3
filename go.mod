module github.com/go-filesystems/s3

// ⛔ THE PATCH, AND THIS REPOSITORY IS ONE OF THE NINE THE FLEET SWEEPS FOR
// EXPOSURE. This line is the only one of the three Go pins a CONSUMER honours:
// `go install` with GOTOOLCHAIN=local obeys it, while the CI pin merely builds
// our artefacts and the toolchain stamped in a binary describes one executable.
//
// Raised to 1.27.4 on 2026-11-15 for GO-2026-6859 — published 2026-11-11,
// CVE-2026-97054, "HTTP/1 server crash due to MaxBytesReader header race in
// net/http", stdlib, fixed in 1.26.11 and 1.27.4. Read at
// https://vuln.go.dev/ID/GO-2026-6859.json rather than quoted from notes.
//
// ⛔ AND HERE IT IS NOT MERELY A CONTRACT: this server speaks HTTP, and the
// advisory is about an HTTP/1 SERVER. The repository is named in
// go-fleettools/fleettools/.github/vulnscan-exposed.txt, the list cut BY
// EXPOSURE rather than by convenience.
//
// ⛔⛔ AND THE RELEASE IS THE FIX, NOT THE MERGE. Measured on 2026-11-15:
// this working tree already declared `go 1.27.3` while the PUBLISHED v0.5.0
// still declared `go 1.27.1` — the branch was two patches ahead of what every
// consumer actually read. A directive raised and not released protects nobody,
// which is why this change is tagged rather than merely merged.
//
// ⛔ Renovate will never offer this edit, measured the same day: its
// github-actions manager maintains the CI pin and its gomod manager SEES this
// directive while proposing nothing for it. Of the three pins, the only one a
// consumer honours is the unwatched one.
go 1.27.4

require (
	github.com/go-filesystems/fat32 v0.13.0
	github.com/go-filesystems/interface v0.6.0
	github.com/go-filesystems/osfs v0.4.0
	github.com/go-volumes/s3 v0.2.2
)

require (
	github.com/go-volumes/gpt v0.3.0 // indirect
	github.com/go-volumes/safeio v0.0.0-20261011024942-5635ea21db3b // indirect
	golang.org/x/sys v0.49.0 // indirect
)
